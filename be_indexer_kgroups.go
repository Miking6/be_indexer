package be_indexer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/echoface/be_indexer/util"
)

type (
	// EntriesContainer for default Entries Holder, it can hold different field's entries,
	// but for ACMatcher or other Holder, it may only hold entries for one field
	EntriesContainer struct {
		defaultHolder EntriesHolder // shared entries holder

		fieldHolder map[BEField]EntriesHolder // special field holder
	}

	KGroupsBEIndex struct {
		indexBase
		kSizeContainers []*EntriesContainer
	}
)

func newFieldEntriesContainer() *EntriesContainer {
	return &EntriesContainer{
		defaultHolder: NewEntriesHolder(HolderNameDefault),
		fieldHolder:   map[BEField]EntriesHolder{},
	}
}

func (c *EntriesContainer) compileEntries() (err error) {
	if err = c.defaultHolder.CompileEntries(); err != nil {
		return err
	}

	for _, holder := range c.fieldHolder {
		if err = holder.CompileEntries(); err != nil {
			return err
		}
	}
	return nil
}

func (c *EntriesContainer) getFieldHolder(desc *FieldDesc) EntriesHolder {
	if desc.Container == HolderNameDefault {
		return c.defaultHolder
	}
	if holder, ok := c.fieldHolder[desc.Field]; ok {
		return holder
	}
	return nil
}

func (c *EntriesContainer) GetHolder(desc *FieldDesc) EntriesHolder {
	if desc.Container == HolderNameDefault {
		return c.defaultHolder
	}
	if holder, ok := c.fieldHolder[desc.Field]; ok {
		return holder
	}
	return nil
}

func (c *EntriesContainer) CreateHolder(desc *FieldDesc) EntriesHolder {
	holder := c.GetHolder(desc)
	if holder != nil { // already has holder for field
		return holder
	}
	hasBuilder := HasHolderBuilder(desc.Container)
	util.PanicIf(!hasBuilder, "field:%s container:%s not found, plz register it", desc.Field, desc.Container)

	holder = NewEntriesHolder(desc.Container)
	c.fieldHolder[desc.Field] = holder

	return holder
}

func (c *EntriesContainer) DumpEntries(buf *strings.Builder) {
	c.defaultHolder.DumpEntries(buf)
	for field, holder := range c.fieldHolder {
		buf.WriteString(fmt.Sprintf("\nfield:%s holder:\n", field))
		holder.DumpEntries(buf)
	}
}

// DumpInfo
// default holder: {name:%s value_count:%d, max_entries:%d avg_entries:%d}
// field holder:
//
//	>field:%s {name: %s, value_count:%d max_entries:%d avg_entries:%d}
//	>field:%s {name: %s, value_count:%d max_entries:%d avg_entries:%d}
func (c *EntriesContainer) DumpInfo(buf *strings.Builder) {
	buf.WriteString("holder information:")
	c.defaultHolder.DumpInfo(buf)
	for field, holder := range c.fieldHolder {
		buf.WriteString(fmt.Sprintf("  >field:%s ", field))
		holder.DumpInfo(buf)
		buf.WriteString("\n")
	}
}

func NewKGroupsBEIndex() BEIndex {
	index := &KGroupsBEIndex{
		indexBase: indexBase{
			fieldsData: make(map[BEField]*FieldDesc),
		},
		kSizeContainers: make([]*EntriesContainer, 0),
	}
	return index
}

func (bi *KGroupsBEIndex) maxK() int {
	return len(bi.kSizeContainers) - 1
}

// newContainer(k int) *EntriesContainer get a container if created, otherwise create one
func (bi *KGroupsBEIndex) newContainer(k int) *EntriesContainer {
	for k >= len(bi.kSizeContainers) {
		container := newFieldEntriesContainer()
		bi.kSizeContainers = append(bi.kSizeContainers, container)
	}
	return bi.kSizeContainers[k]
}

func (bi *KGroupsBEIndex) compileIndexer() (err error) {
	for _, sizeEntries := range bi.kSizeContainers {
		if err = sizeEntries.compileEntries(); err != nil {
			return err
		}
	}
	if bi.wildcardEntries.Len() > 0 {
		sort.Sort(bi.wildcardEntries)
	}
	return nil
}

func (bi *KGroupsBEIndex) getKSizeEntries(k int) *EntriesContainer {
	if k >= len(bi.kSizeContainers) {
		panic(fmt.Errorf("k:[%d] out of range", k))
	}
	return bi.kSizeContainers[k]
}

func (bi *KGroupsBEIndex) initCursors(ctx *retrieveContext, k int) (fCursors FieldCursors, err error) {

	fCursors = make(FieldCursors, 0, len(bi.fieldsData))

	if k == 0 && len(bi.wildcardEntries) > 0 {
		pl := NewEntriesCursor(wildcardQKey, bi.wildcardEntries)
		fCursors = append(fCursors, NewFieldCursor(pl))
	}

	kSizeContainer := bi.getKSizeEntries(k)

	var entriesList EntriesCursors
	var holder EntriesHolder

	var ok bool
	var desc *FieldDesc

	for field, values := range ctx.assigns {

		if desc, ok = bi.fieldsData[field]; !ok {
			// not recognized field, no document care about this field, ignore
			continue
		}

		if holder = kSizeContainer.getFieldHolder(desc); holder == nil {
			// Logger.Debugf("entries holder not found, field:%s", desc.Field)
			// case 1: user/client can pass non-exist assign, so just skip this field
			// case 2: no any document has condition on this field
			continue
		}

		if entriesList, err = holder.GetEntries(desc, values); err != nil {
			Logger.Errorf("fetch entries from holder fail:%s, field:%s\n", err.Error(), desc.Field)
			return nil, err
		}

		if len(entriesList) > 0 {
			fCursors = append(fCursors, NewFieldCursor(entriesList...))
			Logger.Debugf("<%s,%v> fetch %d posting list\n", desc.Field, values, len(entriesList))
		} else {
			Logger.Debugf("<%s,%v> nothing matched from entries holder\n", desc.Field, values)
		}
	}
	return fCursors, nil
}

// retrieveK retrieve matched result from k size index data
func (bi *KGroupsBEIndex) retrieveK(ctx *retrieveContext, fieldCursors FieldCursors, needMatchCnt int) {
	if len(fieldCursors) < needMatchCnt {
		LogInfoIf(ctx.dumpStepInfo, "need match:%d but only:%d", needMatchCnt, len(fieldCursors))
		return
	}
	// sort.Sort(fieldCursors)
	fieldCursors.Sort()

	for !fieldCursors[needMatchCnt-1].GetCurEntryID().IsNULLEntry() {
		if ctx.dumpStepInfo {
			Logger.Infof("round need match:%d continue docs:%v", needMatchCnt, ctx.collector.GetDocIDs())
		}
		if ctx.dumpEntriesDetail {
			Logger.Infof("round need match:%d continue entries\n%s", needMatchCnt, fieldCursors.Dump())
		}

		eid := fieldCursors[0].GetCurEntryID()
		endEID := fieldCursors[needMatchCnt-1].GetCurEntryID()

		conjID := eid.GetConjID()
		endConjID := endEID.GetConjID()

		nextID := NewEntryID(endConjID, false) // 逻辑按照conjID执行，但是直接使用endEID可能跳过排除逻辑的EID

		if conjID == endConjID {

			// nextID = endEID + 1
			nextID = NewEntryID(endEID.GetConjID(), true) + 1

			if eid.IsInclude() {

				ctx.collector.Add(conjID.DocID(), conjID)

			} else { //exclude
				for i := needMatchCnt; i < len(fieldCursors); i++ {
					if fieldCursors[i].GetCurEntryID() < nextID {
						fieldCursors[i].SkipTo(nextID)
					}
				}
			}
		}
		for i := 0; i < needMatchCnt; i++ { // 推进游标
			fieldCursors[i].SkipTo(nextID)
		}

		fieldCursors.Sort()
		// sort.Sort(fieldCursors)
		if ctx.dumpStepInfo {
			Logger.Infof("round end need match:%d, docs:%v", needMatchCnt, ctx.collector.GetDocIDs())
		}
	}
}

func (bi *KGroupsBEIndex) Retrieve(
	queries Assignments, opts ...IndexOpt) (result DocIDList, err error) {

	collector := PickCollector()

	defer PutCollector(collector)

	if err = bi.RetrieveWithCollector(queries, collector, opts...); err != nil {
		return nil, err
	}

	result = collector.GetDocIDs()

	return result, nil
}

func (bi *KGroupsBEIndex) RetrieveWithCollector(
	queries Assignments, collector ResultCollector, opts ...IndexOpt) (err error) {

	ctx := newRetrieveCtx(queries, opts...)
	util.PanicIf(ctx.collector != nil, "can't specify collector twice")

	ctx.collector = collector

	var fCursors FieldCursors
	for k := util.MinInt(queries.Size(), bi.maxK()); k >= 0; k-- {
		if fCursors, err = bi.initCursors(&ctx, k); err != nil {
			return err
		}
		LogInfoIf(ctx.dumpStepInfo, "start@step:%d cursors:%d", k, len(fCursors))
		if ctx.dumpEntriesDetail {
			Logger.Infof("RetrieveWithCollector k:%d initial entries:\n%s", k, fCursors.Dump())
		}

		needMatchCnt := util.MaxInt(k, 1)
		bi.retrieveK(&ctx, fCursors, needMatchCnt)
	}
	return nil
}

func (bi *KGroupsBEIndex) DumpEntries(sb *strings.Builder) {
	sb.WriteString("\n+++++++ size grouped boolean indexing entries +++++++++++ \n")
	sb.WriteString(fmt.Sprintf(">>Z:\n"))
	sb.WriteString(wildcardQKey.String())
	sb.WriteString(":")
	sb.WriteString(fmt.Sprintf("%v", bi.wildcardEntries.DocString()))
	sb.WriteString("\n")
	for size, container := range bi.kSizeContainers {
		sb.WriteString(fmt.Sprintf(">>K:%d\n", size))
		container.DumpEntries(sb)
		sb.WriteString("\n")
	}
	sb.WriteString("+++++++++++++size grouped index entries end +++++++++++++++++\n")
}

func (bi *KGroupsBEIndex) DumpIndexInfo(sb *strings.Builder) {
	sb.WriteString("\n+++++++ size grouped boolean indexing info +++++++++++\n")
	sb.WriteString(fmt.Sprintf("wildcard info: count:%d\n", len(bi.wildcardEntries)))
	for k, c := range bi.kSizeContainers {
		sb.WriteString(fmt.Sprintf(">> container for size k:%d\n", k))
		c.DumpInfo(sb)
		sb.WriteString("\n")
	}
	sb.WriteString("++++++++++++ size grouped index info end ++++++++++++++++++\n")
}
