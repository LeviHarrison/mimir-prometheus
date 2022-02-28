package tsdb

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// TestOOOHeadIndexReader_Series tests that the Series method works as expected.
// However it does so by creating chunks and memory mapping them unlike other
// tests of the head where samples are appended and we let the head memory map.
// We do this because the ingestion path and the appender for out of order
// samples are not ready yet.
func TestOOOHeadIndexReader_Series(t *testing.T) {
	tests := []struct {
		name                string
		queryMinT           int64
		queryMaxT           int64
		inputChunkIntervals []struct{ mint, maxt int64 }
		expSeriesError      bool
		expChunks           []chunks.Meta
	}{
		{
			name:           "Empty result and no error when head is empty",
			queryMinT:      0,
			queryMaxT:      100,
			expSeriesError: false,
			expChunks:      nil,
		},
		{
			name:      "If query interval is bigger than the existing chunks nothing is returned",
			queryMinT: 500,
			queryMaxT: 700,
			inputChunkIntervals: []struct{ mint, maxt int64 }{
				{100, 400},
			},
			expSeriesError: false,
			// ts                    0       100       150       200       250       300       350       400       450       500       550       600       650       700
			// Query Interval                                                                                                  [---------------------------------------]
			// Chunk 0: 0x1000000              [-----------------------------------------------------------]
			// Expected Output  Empty
			expChunks: nil,
		},
		{
			name:      "When there are overlapping chunks, only the references of the first overlapping chunks are returned",
			queryMinT: 0,
			queryMaxT: 700,
			inputChunkIntervals: []struct{ mint, maxt int64 }{
				{100, 200},
				{500, 600},
				{150, 250},
				{550, 650},
			},
			expSeriesError: false,
			// ts                    0       100       150       200       250       300       350       400       450       500       550       600       650       700
			// Query Interval        [---------------------------------------------------------------------------------------------------------------------------------]
			// Chunk 0: 0x1000000              [-------------------]
			// Chunk 1: 0x1000001                                                                                              [-------------------]
			// Chunk 2: 0x1000002                        [-------------------]
			// Chunk 3: 0x1000003                                                                                                        [-------------------]
			// Expected Output  [0x1000000, 0x1000001] with OOOLastReferences pointing to 0x1000003
			expChunks: []chunks.Meta{
				{Ref: 0x1000000, Chunk: chunkenc.Chunk(nil), MinTime: 100, MaxTime: 200, OOOLastRef: 0x1000003, OOOLastMinTime: 550, OOOLastMaxTime: 650},
				{Ref: 0x1000001, Chunk: chunkenc.Chunk(nil), MinTime: 500, MaxTime: 600, OOOLastRef: 0x1000003, OOOLastMinTime: 550, OOOLastMaxTime: 650},
			},
		},
		{
			name:      "If multiple chunks and none overlap, all are returned",
			queryMinT: 0,
			queryMaxT: 700,
			inputChunkIntervals: []struct{ mint, maxt int64 }{
				{100, 200},
				{200, 300},
				{300, 400},
				{400, 500},
			},
			expSeriesError: false,
			// ts                    0       100       150       200       250       300       350       400       450       500       550       600       650       700
			// Query Interval        [---------------------------------------------------------------------------------------------------------------------------------]
			// Chunk 0: 0x1000000              [-------------------]
			// Chunk 1: 0x1000001                                  [-------------------]
			// Chunk 2: 0x1000002                                                      [-------------------]
			// Chunk 3: 0x1000003                                                                          [------------------]
			// Expected Output  [0x1000000, 0x1000001, 0x1000002, 0x1000003] with OOOLastReferences pointing to 0x1000003
			expChunks: []chunks.Meta{
				{Ref: 0x1000000, Chunk: chunkenc.Chunk(nil), MinTime: 100, MaxTime: 200, OOOLastRef: 0x1000003, OOOLastMinTime: 400, OOOLastMaxTime: 500},
				{Ref: 0x1000001, Chunk: chunkenc.Chunk(nil), MinTime: 200, MaxTime: 300, OOOLastRef: 0x1000003, OOOLastMinTime: 400, OOOLastMaxTime: 500},
				{Ref: 0x1000002, Chunk: chunkenc.Chunk(nil), MinTime: 300, MaxTime: 400, OOOLastRef: 0x1000003, OOOLastMinTime: 400, OOOLastMaxTime: 500},
				{Ref: 0x1000003, Chunk: chunkenc.Chunk(nil), MinTime: 400, MaxTime: 500, OOOLastRef: 0x1000003, OOOLastMinTime: 400, OOOLastMaxTime: 500},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHead(t, 1000, false)
			defer func() {
				require.NoError(t, h.Close())
			}()
			require.NoError(t, h.Init(0))

			s1Lset := labels.FromStrings("foo", "bar")
			s1ID := uint64(1)
			s1, _, _ := h.getOrCreate(s1ID, s1Lset)

			for _, ic := range tc.inputChunkIntervals {
				s1.oooMmappedChunks = append(s1.oooMmappedChunks, &mmappedChunk{
					minTime: ic.mint,
					maxTime: ic.maxt,
				})
			}

			ir := NewOOOHeadIndexReader(h, tc.queryMinT, tc.queryMaxT)

			var chks []chunks.Meta
			var respLset labels.Labels
			err := ir.Series(storage.SeriesRef(s1ID), &respLset, &chks)
			if tc.expSeriesError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, s1Lset, respLset)
			require.Equal(t, tc.expChunks, chks)
		})
	}
}
