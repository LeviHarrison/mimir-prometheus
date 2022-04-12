package tsdb

import (
	"math"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/index"
)

var _ IndexReader = &OOOHeadIndexReader{}

// OOOHeadIndexReader implements IndexReader so ooo samples in the head can be
// accessed.
// It also has a reference to headIndexReader so we can leverage on its
// IndexReader implementation for all the methods that remain the same. We
// decided to do this to avoid code duplication.
// The only methods that change are the ones about getting Series and Postings.
type OOOHeadIndexReader struct {
	*headIndexReader // A reference to the headIndexReader so we can reuse as many interface implementation as possible.
}

func NewOOOHeadIndexReader(head *Head, mint, maxt int64) *OOOHeadIndexReader {
	hr := &headIndexReader{
		head: head,
		mint: mint,
		maxt: maxt,
	}
	return &OOOHeadIndexReader{hr}
}

func (oh *OOOHeadIndexReader) Series(ref storage.SeriesRef, lbls *labels.Labels, chks *[]chunks.Meta) error {
	s := oh.head.series.getByID(chunks.HeadSeriesRef(ref))

	if s == nil {
		oh.head.metrics.seriesNotFound.Inc()
		return storage.ErrNotFound
	}
	*lbls = append((*lbls)[:0], s.lset...)

	if chks == nil {
		return nil
	}

	s.Lock()
	defer s.Unlock()
	*chks = (*chks)[:0]

	tmpChks := make([]chunks.Meta, 0, len(s.oooMmappedChunks))

	// We define these markers to track the last chunk reference while we
	// fill the chunk meta.
	// These markers are useful to give consistent responses to repeated queries
	// even if new chunks that might be overlapping or not are added afterwards.
	// Also, lastMinT and lastMaxT are initialized to the max int as a sentinel
	// value to know they are unset.
	var lastChunkRef chunks.ChunkRef
	lastMinT, lastMaxT := int64(math.MaxInt64), int64(math.MaxInt64)

	addChunk := func(minT, maxT int64, ref chunks.ChunkRef) {
		// the first time we get called is for the last included chunk.
		// set the markers accordingly
		if lastMinT == int64(math.MaxInt64) {
			lastChunkRef = ref
			lastMinT = minT
			lastMaxT = maxT
		}

		tmpChks = append(tmpChks, chunks.Meta{
			MinTime:        minT,
			MaxTime:        maxT,
			Ref:            ref,
			OOOLastRef:     lastChunkRef,
			OOOLastMinTime: lastMinT,
			OOOLastMaxTime: lastMaxT,
		})
	}

	// Collect all chunks that overlap the query range, in order from most recent to most old,
	// so we can set the correct markers.
	if s.oooHeadChunk != nil {
		c := s.oooHeadChunk
		if c.OverlapsClosedInterval(oh.mint, oh.maxt) {
			ref := chunks.ChunkRef(chunks.NewHeadChunkRef(s.ref, s.oooHeadChunkID(len(s.oooMmappedChunks))))
			addChunk(c.minTime, c.maxTime, ref)
		}
	}
	for i := len(s.oooMmappedChunks) - 1; i >= 0; i-- {
		c := s.oooMmappedChunks[i]
		if c.OverlapsClosedInterval(oh.mint, oh.maxt) {
			ref := chunks.ChunkRef(chunks.NewHeadChunkRef(s.ref, s.oooHeadChunkID(i)))
			addChunk(c.minTime, c.maxTime, ref)
		}
	}

	// There is nothing to do if we did not collect any chunk
	if len(tmpChks) == 0 {
		return nil
	}

	// Next we want to sort all the collected chunks by min time so we can find
	// those that overlap.
	sort.Sort(byMinTime(tmpChks))

	// Next we want to iterate the sorted collected chunks and only return the
	// chunks Meta the first chunk that overlaps with others.
	// Example chunks of a series: 5:(100, 200) 6:(500, 600) 7:(150, 250) 8:(550, 650)
	// In the example 5 overlaps with 7 and 6 overlaps with 8 so we only want to
	// to return chunk Metas for chunk 5 and chunk 6
	*chks = append(*chks, tmpChks[0])
	maxTime := tmpChks[0].MaxTime // tracks the maxTime of the previous "to be merged chunk"
	for _, c := range tmpChks[1:] {
		if c.MinTime > maxTime {
			*chks = append(*chks, c)
			maxTime = c.MaxTime
		} else if c.MaxTime > maxTime {
			maxTime = c.MaxTime
			(*chks)[len(*chks)-1].MaxTime = c.MaxTime
		}
	}

	return nil
}

type byMinTime []chunks.Meta

func (b byMinTime) Len() int           { return len(b) }
func (b byMinTime) Less(i, j int) bool { return b[i].MinTime < b[j].MinTime }
func (b byMinTime) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

func (oh *OOOHeadIndexReader) Postings(name string, values ...string) (index.Postings, error) {
	switch len(values) {
	case 0:
		return index.EmptyPostings(), nil
	case 1:
		return oh.head.postings.Get(name, values[0]), nil // TODO(ganesh) Also call GetOOOPostings
	default:
		// TODO(ganesh) We want to only return postings for out of order series.
		res := make([]index.Postings, 0, len(values))
		for _, value := range values {
			res = append(res, oh.head.postings.Get(name, value)) // TODO(ganesh) Also call GetOOOPostings
		}
		return index.Merge(res...), nil
	}
}

type OOOHeadChunkReader struct {
	head       *Head
	mint, maxt int64
}

func NewOOOHeadChunkReader(head *Head, mint, maxt int64) *OOOHeadChunkReader {
	return &OOOHeadChunkReader{
		head: head,
		mint: mint,
		maxt: maxt,
	}
}

func (cr OOOHeadChunkReader) Chunk(ref chunks.ChunkRef) (chunkenc.Chunk, error) {
	sid, cid := chunks.HeadChunkRef(ref).Unpack()

	s := cr.head.series.getByID(sid)
	// This means that the series has been garbage collected.
	if s == nil {
		return nil, storage.ErrNotFound
	}

	s.Lock()
	c, garbageCollect, err := s.ooochunk(cid, cr.head.chunkDiskMapper) // TODO(jesus.vazquez) here is where we do the magic of merging overlapping chunks
	if err != nil {
		s.Unlock()
		return nil, err
	}
	defer func() {
		if garbageCollect {
			// Set this to nil so that Go GC can collect it after it has been used.
			c.chunk = nil
			s.memChunkPool.Put(c)
		}
	}()

	// TODO(jesus.vazquez) I wonder if this check should be run here
	// This means that the chunk is outside the specified range.
	if !c.OverlapsClosedInterval(cr.mint, cr.maxt) {
		s.Unlock()
		return nil, storage.ErrNotFound
	}
	s.Unlock()

	return &safeChunk{
		Chunk:           c.chunk,
		s:               s,
		cid:             cid,
		chunkDiskMapper: cr.head.chunkDiskMapper,
	}, nil
}

func (cr OOOHeadChunkReader) Close() error {
	return nil
}
