// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package storepb

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/thanos-io/thanos/pkg/testutil/custom"

	"github.com/efficientgo/core/testutil"
	"github.com/pkg/errors"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
)

type testStoreServer struct {
	series        []*SeriesResponse
	seriesLastReq *SeriesRequest

	labelNames        *LabelNamesResponse
	labelNamesLastReq *LabelNamesRequest

	labelValues        *LabelValuesResponse
	labelValuesLastReq *LabelValuesRequest

	err error
}

func (t *testStoreServer) Series(r *SeriesRequest, server Store_SeriesServer) error {
	t.seriesLastReq = r
	for i, s := range t.series {
		if t.err != nil && i == len(t.series)/2 {
			return t.err
		}
		if err := server.Send(s); err != nil {
			return err
		}
	}
	return nil
}

func (t *testStoreServer) LabelNames(_ context.Context, r *LabelNamesRequest) (*LabelNamesResponse, error) {
	t.labelNamesLastReq = r
	return t.labelNames, t.err
}

func (t *testStoreServer) LabelValues(_ context.Context, r *LabelValuesRequest) (*LabelValuesResponse, error) {
	t.labelValuesLastReq = r
	return t.labelValues, t.err
}

func TestServerAsClient(t *testing.T) {
	defer custom.TolerantVerifyLeak(t)

	ctx := context.Background()
	t.Run("Series", func(t *testing.T) {
		s := &testStoreServer{
			series: []*SeriesResponse{
				NewSeriesResponse(&Series{
					Labels: []labelpb.ZLabel{{Name: "a", Value: "b"}},
					Chunks: []AggrChunk{{MinTime: 123, MaxTime: 124}, {MinTime: 12455, MaxTime: 14124}},
				}),
				NewSeriesResponse(&Series{
					Labels: []labelpb.ZLabel{{Name: "a", Value: "b1"}},
					Chunks: []AggrChunk{{MinTime: 1231, MaxTime: 124}, {MinTime: 12455, MaxTime: 14124}},
				}),
				NewWarnSeriesResponse(errors.New("yolo")),
				NewSeriesResponse(&Series{
					Labels: []labelpb.ZLabel{{Name: "a", Value: "b3"}},
					Chunks: []AggrChunk{{MinTime: 123, MaxTime: 124}, {MinTime: 124554, MaxTime: 14124}},
				}),
			}}
		t.Run("ok", func(t *testing.T) {
			for i := 0; i < 20; i++ {
				r := &SeriesRequest{
					MinTime:                 -214,
					MaxTime:                 213,
					Matchers:                []LabelMatcher{{Value: "wfsdfs", Name: "__name__", Type: LabelMatcher_EQ}},
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				client, err := ServerAsClient(s).Series(ctx, r)
				testutil.Ok(t, err)
				var resps []*SeriesResponse
				for {
					resp, err := client.Recv()
					if err == io.EOF {
						break
					}
					testutil.Ok(t, err)
					resps = append(resps, resp)
				}
				testutil.Equals(t, s.series, resps)
				testutil.Equals(t, r, s.seriesLastReq)
				s.seriesLastReq = nil
			}
		})
		t.Run("ok, close send", func(t *testing.T) {
			s.err = errors.New("some error")
			for i := 0; i < 20; i++ {
				r := &SeriesRequest{
					MinTime:                 -214,
					MaxTime:                 213,
					Matchers:                []LabelMatcher{{Value: "wfsdfs", Name: "__name__", Type: LabelMatcher_EQ}},
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				client, err := ServerAsClient(s).Series(ctx, r)
				testutil.Ok(t, err)
				var resps []*SeriesResponse
				for {
					if len(resps) == len(s.series)/2 {
						testutil.Ok(t, client.CloseSend())
						break
					}
					resp, err := client.Recv()
					if err == io.EOF {
						break
					}
					testutil.Ok(t, err)
					resps = append(resps, resp)
				}
				testutil.Equals(t, s.series[:len(s.series)/2], resps)
				testutil.Equals(t, r, s.seriesLastReq)
				s.seriesLastReq = nil
			}
		})
		t.Run("error", func(t *testing.T) {
			for i := 0; i < 20; i++ {
				r := &SeriesRequest{
					MinTime:                 -214,
					MaxTime:                 213,
					Matchers:                []LabelMatcher{{Value: "wfsdfs", Name: "__name__", Type: LabelMatcher_EQ}},
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				client, err := ServerAsClient(s).Series(ctx, r)
				testutil.Ok(t, err)
				var resps []*SeriesResponse
				for {
					resp, err := client.Recv()
					if err == io.EOF {
						break
					}
					if err == s.err {
						break
					}
					testutil.Ok(t, err)
					resps = append(resps, resp)
				}
				testutil.Equals(t, s.series[:len(s.series)/2], resps)
				testutil.Equals(t, r, s.seriesLastReq)
				s.seriesLastReq = nil
			}
		})
		t.Run("race", func(t *testing.T) {
			s.err = nil
			for i := 0; i < 20; i++ {
				r := &SeriesRequest{
					MinTime:                 -214,
					MaxTime:                 213,
					Matchers:                []LabelMatcher{{Value: "wfsdfs", Name: "__name__", Type: LabelMatcher_EQ}},
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				client, err := ServerAsClient(s).Series(ctx, r)
				testutil.Ok(t, err)
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						_, err := client.Recv()
						if err != nil {
							break
						}
					}
				}()
				testutil.Ok(t, client.CloseSend())
				wg.Wait()
				s.seriesLastReq = nil
			}
		})
	})
	t.Run("LabelNames", func(t *testing.T) {
		s := &testStoreServer{}
		t.Run("ok", func(t *testing.T) {
			for i := 0; i < 20; i++ {
				r := &LabelNamesRequest{
					Start:                   -1,
					End:                     234,
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				resp, err := ServerAsClient(s).LabelNames(ctx, r)
				testutil.Ok(t, err)
				testutil.Equals(t, s.labelNames, resp)
				testutil.Equals(t, r, s.labelNamesLastReq)
				s.labelNamesLastReq = nil
			}
		})
		t.Run("error", func(t *testing.T) {
			s.err = errors.New("some error")
			for i := 0; i < 20; i++ {
				r := &LabelNamesRequest{
					Start:                   -1,
					End:                     234,
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				_, err := ServerAsClient(s).LabelNames(ctx, r)
				testutil.NotOk(t, err)
				testutil.Equals(t, s.err, err)
			}
		})
	})
	t.Run("LabelValues", func(t *testing.T) {
		s := &testStoreServer{
			labelValues: &LabelValuesResponse{
				Warnings: []string{"1", "a"},
				Values:   []string{"abc1", "go_goroutines"},
			},
		}
		t.Run("ok", func(t *testing.T) {
			for i := 0; i < 20; i++ {
				r := &LabelValuesRequest{
					Label:                   "__name__",
					Start:                   -1,
					End:                     234,
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				resp, err := ServerAsClient(s).LabelValues(ctx, r)
				testutil.Ok(t, err)
				testutil.Equals(t, s.labelValues, resp)
				testutil.Equals(t, r, s.labelValuesLastReq)
				s.labelValuesLastReq = nil
			}
		})
		t.Run("error", func(t *testing.T) {
			s.err = errors.New("some error")
			for i := 0; i < 20; i++ {
				r := &LabelValuesRequest{
					Label:                   "__name__",
					Start:                   -1,
					End:                     234,
					PartialResponseStrategy: PartialResponseStrategy_ABORT,
				}
				_, err := ServerAsClient(s).LabelValues(ctx, r)
				testutil.NotOk(t, err)
				testutil.Equals(t, s.err, err)
			}
		})
	})
}
