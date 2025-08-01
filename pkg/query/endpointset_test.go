// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/efficientgo/core/testutil"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/info/infopb"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
)

var testGRPCOpts = []grpc.DialOption{
	grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32)),
	grpc.WithTransportCredentials(insecure.NewCredentials()),
}

var (
	sidecarInfo = &infopb.InfoResponse{
		ComponentType: component.Sidecar.String(),
		Store: &infopb.StoreInfo{
			MinTime: math.MinInt64,
			MaxTime: math.MaxInt64,
		},
		Exemplars:      &infopb.ExemplarsInfo{},
		Rules:          &infopb.RulesInfo{},
		MetricMetadata: &infopb.MetricMetadataInfo{},
		Targets:        &infopb.TargetsInfo{},
	}
	queryInfo = &infopb.InfoResponse{
		ComponentType: component.Query.String(),
		Store: &infopb.StoreInfo{
			MinTime: math.MinInt64,
			MaxTime: math.MaxInt64,
		},
		Exemplars:      &infopb.ExemplarsInfo{},
		Rules:          &infopb.RulesInfo{},
		MetricMetadata: &infopb.MetricMetadataInfo{},
		Targets:        &infopb.TargetsInfo{},
		Query:          &infopb.QueryAPIInfo{},
	}
	ruleInfo = &infopb.InfoResponse{
		ComponentType: component.Rule.String(),
		Store: &infopb.StoreInfo{
			MinTime: math.MinInt64,
			MaxTime: math.MaxInt64,
		},
		Rules: &infopb.RulesInfo{},
	}
	storeGWInfo = &infopb.InfoResponse{
		ComponentType: component.Store.String(),
		Store: &infopb.StoreInfo{
			MinTime: math.MinInt64,
			MaxTime: math.MaxInt64,
		},
	}
	receiveInfo = &infopb.InfoResponse{
		ComponentType: component.Receive.String(),
		Store: &infopb.StoreInfo{
			MinTime: math.MinInt64,
			MaxTime: math.MaxInt64,
		},
		Exemplars: &infopb.ExemplarsInfo{},
	}
)

type mockedEndpoint struct {
	infoDelay time.Duration
	info      infopb.InfoResponse
	err       error
}

func (c *mockedEndpoint) setResponseError(err error) {
	c.err = err
}

func (c *mockedEndpoint) Info(ctx context.Context, r *infopb.InfoRequest) (*infopb.InfoResponse, error) {
	if c.err != nil {
		return nil, c.err
	}

	select {
	case <-ctx.Done():
		return nil, context.Canceled
	case <-time.After(c.infoDelay):
	}

	return &c.info, nil
}

type APIs struct {
	store          bool
	metricMetadata bool
	rules          bool
	target         bool
	exemplars      bool
}

type testEndpointMeta struct {
	*infopb.InfoResponse
	extlsetFn func(addr string) []labelpb.ZLabelSet
	infoDelay time.Duration
	err       error
}

type testEndpoints struct {
	srvs        map[string]*grpc.Server
	endpoints   map[string]*mockedEndpoint
	orderAddrs  []string
	exposedAPIs map[string]*APIs
}

func startTestEndpoints(testEndpointMeta []testEndpointMeta) (*testEndpoints, error) {
	e := &testEndpoints{
		srvs:        map[string]*grpc.Server{},
		endpoints:   map[string]*mockedEndpoint{},
		exposedAPIs: map[string]*APIs{},
	}

	for _, meta := range testEndpointMeta {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			// Close so far started servers.
			e.Close()
			return nil, err
		}

		srv := grpc.NewServer()
		addr := listener.Addr().String()

		endpointSrv := &mockedEndpoint{
			err: meta.err,
			info: infopb.InfoResponse{
				LabelSets:      meta.extlsetFn(listener.Addr().String()),
				Store:          meta.Store,
				MetricMetadata: meta.MetricMetadata,
				Rules:          meta.Rules,
				Targets:        meta.Targets,
				Exemplars:      meta.Exemplars,
				Query:          meta.Query,
				ComponentType:  meta.ComponentType,
			},
			infoDelay: meta.infoDelay,
		}
		infopb.RegisterInfoServer(srv, endpointSrv)
		go func() {
			_ = srv.Serve(listener)
		}()

		e.exposedAPIs[addr] = exposedAPIs(meta.ComponentType)
		e.srvs[addr] = srv
		e.endpoints[addr] = endpointSrv
		e.orderAddrs = append(e.orderAddrs, listener.Addr().String())
	}

	return e, nil
}

func (e *testEndpoints) EndpointAddresses() []string {
	var endpoints []string
	endpoints = append(endpoints, e.orderAddrs...)
	return endpoints
}

func (e *testEndpoints) Close() {
	for _, srv := range e.srvs {
		srv.Stop()
	}
	e.srvs = nil
}

func (e *testEndpoints) CloseOne(addr string) {
	srv, ok := e.srvs[addr]
	if !ok {
		return
	}

	srv.Stop()
	delete(e.srvs, addr)
}

func TestTruncateExtLabels(t *testing.T) {
	t.Parallel()

	const testLength = 10

	for _, tc := range []struct {
		labelToTruncate string
		expectedOutput  string
	}{
		{
			labelToTruncate: "{abc}",
			expectedOutput:  "{abc}",
		},
		{
			labelToTruncate: "{abcdefgh}",
			expectedOutput:  "{abcdefgh}",
		},
		{
			labelToTruncate: "{abcdefghij}",
			expectedOutput:  "{abcdefgh}",
		},
		{
			labelToTruncate: "{abcde花}",
			expectedOutput:  "{abcde花}",
		},
		{
			labelToTruncate: "{abcde花朵}",
			expectedOutput:  "{abcde花}",
		},
		{
			labelToTruncate: "{abcde花fghij}",
			expectedOutput:  "{abcde花}",
		},
	} {
		t.Run(tc.labelToTruncate, func(t *testing.T) {
			got := truncateExtLabels(tc.labelToTruncate, testLength)
			testutil.Equals(t, tc.expectedOutput, got)
			testutil.Assert(t, len(got) <= testLength)
		})
	}
}

func TestEndpointSetUpdate(t *testing.T) {
	t.Parallel()

	const metricsMeta = `
	# HELP thanos_store_nodes_grpc_connections Number of gRPC connection to Store APIs. Opened connection means healthy store APIs available for Querier.
	# TYPE thanos_store_nodes_grpc_connections gauge
	`
	testCases := []struct {
		name       string
		endpoints  []testEndpointMeta
		strict     bool
		connLabels []string

		expectedEndpoints   int
		expectedConnMetrics string
	}{
		{
			name: "available endpoint",
			endpoints: []testEndpointMeta{
				{
					InfoResponse: sidecarInfo,
					extlsetFn: func(addr string) []labelpb.ZLabelSet {
						return labelpb.ZLabelSetsFromPromLabels(
							labels.FromStrings("addr", addr, "a", "b"),
						)
					},
				},
			},
			connLabels: []string{"store_type"},

			expectedEndpoints: 1,
			expectedConnMetrics: metricsMeta +
				`
			thanos_store_nodes_grpc_connections{store_type="sidecar"} 1
			`,
		},
		{
			name: "unavailable endpoint",
			endpoints: []testEndpointMeta{
				{
					err:          fmt.Errorf("endpoint unavailable"),
					InfoResponse: sidecarInfo,
					extlsetFn: func(addr string) []labelpb.ZLabelSet {
						return labelpb.ZLabelSetsFromPromLabels(
							labels.FromStrings("addr", addr, "a", "b"),
						)
					},
				},
			},

			expectedEndpoints:   0,
			expectedConnMetrics: "",
		},
		{
			name: "slow endpoint",
			endpoints: []testEndpointMeta{
				{
					infoDelay:    5 * time.Second,
					InfoResponse: sidecarInfo,
					extlsetFn: func(addr string) []labelpb.ZLabelSet {
						return labelpb.ZLabelSetsFromPromLabels(
							labels.FromStrings("addr", addr, "a", "b"),
						)
					},
				},
			},

			expectedEndpoints:   0,
			expectedConnMetrics: "",
		},
		{
			name: "strict endpoint",
			endpoints: []testEndpointMeta{
				{
					InfoResponse: sidecarInfo,
					extlsetFn: func(addr string) []labelpb.ZLabelSet {
						return labelpb.ZLabelSetsFromPromLabels(
							labels.FromStrings("addr", addr, "a", "b"),
						)
					},
				},
			},
			strict:            true,
			connLabels:        []string{"store_type"},
			expectedEndpoints: 1,
			expectedConnMetrics: metricsMeta +
				`
			thanos_store_nodes_grpc_connections{store_type="sidecar"} 1
			`,
		},
		{
			name: "long external labels",
			endpoints: []testEndpointMeta{
				{
					InfoResponse: sidecarInfo,
					// Simulate very long external labels.
					extlsetFn: func(addr string) []labelpb.ZLabelSet {
						sLabel := []string{}
						for i := 0; i < 1000; i++ {
							sLabel = append(sLabel, "lbl")
							sLabel = append(sLabel, "val")
						}
						return labelpb.ZLabelSetsFromPromLabels(
							labels.FromStrings(sLabel...),
						)
					},
				},
			},
			expectedEndpoints: 1,
			expectedConnMetrics: metricsMeta + `
			thanos_store_nodes_grpc_connections{external_labels="{lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val\", lbl=\"val}",store_type="sidecar"} 1
			`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			endpoints, err := startTestEndpoints(tc.endpoints)
			testutil.Ok(t, err)
			defer endpoints.Close()

			discoveredEndpointAddr := endpoints.EndpointAddresses()
			// Specify only "store_type" to exclude "external_labels".
			endpointSet := makeEndpointSet(discoveredEndpointAddr, tc.strict, time.Now, tc.connLabels...)
			defer endpointSet.Close()

			endpointSet.Update(context.Background())
			testutil.Equals(t, tc.expectedEndpoints, len(endpointSet.GetEndpointStatus()))
			testutil.Equals(t, tc.expectedEndpoints, len(endpointSet.GetStoreClients()))

			testutil.Ok(t, promtestutil.CollectAndCompare(endpointSet.endpointsMetric, strings.NewReader(tc.expectedConnMetrics)))
		})
	}
}

func TestEndpointSetUpdate_DuplicateSpecs(t *testing.T) {
	t.Parallel()

	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return labelpb.ZLabelSetsFromPromLabels(
					labels.FromStrings("addr", addr, "a", "b"),
				)
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoints.Close()

	discoveredEndpointAddr := endpoints.EndpointAddresses()
	discoveredEndpointAddr = append(discoveredEndpointAddr, discoveredEndpointAddr[0])

	endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
	defer endpointSet.Close()

	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.endpoints))
}

func TestEndpointSetUpdate_EndpointGoingAway(t *testing.T) {
	t.Parallel()

	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return labelpb.ZLabelSetsFromPromLabels(
					labels.FromStrings("addr", addr, "a", "b"),
				)
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoints.Close()

	discoveredEndpointAddr := endpoints.EndpointAddresses()
	endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
	defer endpointSet.Close()

	// Initial update.
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, 1, len(endpointSet.GetStoreClients()))

	endpoints.CloseOne(discoveredEndpointAddr[0])
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, 0, len(endpointSet.GetStoreClients()))
}

func TestEndpointSetUpdate_EndpointComingOnline(t *testing.T) {
	t.Parallel()

	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			err:          fmt.Errorf("endpoint unavailable"),
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return nil
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoints.Close()

	discoveredEndpointAddr := endpoints.EndpointAddresses()
	endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
	defer endpointSet.Close()

	// Initial update.
	endpointSet.Update(context.Background())
	testutil.Equals(t, 0, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, 0, len(endpointSet.GetStoreClients()))

	srvAddr := discoveredEndpointAddr[0]
	endpoints.endpoints[srvAddr].setResponseError(nil)
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, 1, len(endpointSet.GetStoreClients()))
}

func TestEndpointSetUpdate_StrictEndpointMetadata(t *testing.T) {
	t.Parallel()

	infoCopy := *sidecarInfo
	infoCopy.Store = &infopb.StoreInfo{
		MinTime: 111,
		MaxTime: 222,
	}
	info := &infoCopy
	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			err:          fmt.Errorf("endpoint unavailable"),
			InfoResponse: info,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return nil
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoints.Close()

	discoveredEndpointAddr := endpoints.EndpointAddresses()
	endpointSet := makeEndpointSet(discoveredEndpointAddr, true, time.Now)
	defer endpointSet.Close()

	addr := discoveredEndpointAddr[0]
	// Initial update.
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, int64(math.MinInt64), endpointSet.endpoints[addr].metadata.Store.MinTime)
	testutil.Equals(t, int64(math.MaxInt64), endpointSet.endpoints[addr].metadata.Store.MaxTime)

	endpoints.endpoints[addr].setResponseError(nil)
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, info.Store.MinTime, endpointSet.endpoints[addr].metadata.Store.MinTime)
	testutil.Equals(t, info.Store.MaxTime, endpointSet.endpoints[addr].metadata.Store.MaxTime)

	endpoints.CloseOne(addr)
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, info.Store.MinTime, endpointSet.endpoints[addr].metadata.Store.MinTime)
	testutil.Equals(t, info.Store.MaxTime, endpointSet.endpoints[addr].metadata.Store.MaxTime)
}

func TestEndpointSetUpdate_PruneInactiveEndpoints(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		endpoints []testEndpointMeta
		strict    bool

		expectedEndpoints int
	}{
		{
			name:   "non-strict endpoint",
			strict: false,
			endpoints: []testEndpointMeta{
				{
					InfoResponse: sidecarInfo,
					extlsetFn: func(addr string) []labelpb.ZLabelSet {
						return labelpb.ZLabelSetsFromPromLabels(
							labels.FromStrings("addr", addr, "a", "b"),
						)
					},
				},
			},
			expectedEndpoints: 0,
		},
		{
			name:   "strict endpoint",
			strict: true,
			endpoints: []testEndpointMeta{
				{
					InfoResponse: sidecarInfo,
					extlsetFn: func(addr string) []labelpb.ZLabelSet {
						return labelpb.ZLabelSetsFromPromLabels(
							labels.FromStrings("addr", addr, "a", "b"),
						)
					},
				},
			},
			expectedEndpoints: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			endpoints, err := startTestEndpoints(tc.endpoints)
			testutil.Ok(t, err)
			defer endpoints.Close()

			updateTime := time.Now()
			discoveredEndpointAddr := endpoints.EndpointAddresses()
			endpointSet := makeEndpointSet(discoveredEndpointAddr, tc.strict, func() time.Time { return updateTime })
			defer endpointSet.Close()

			endpointSet.Update(context.Background())
			testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
			testutil.Equals(t, 1, len(endpointSet.GetStoreClients()))

			addr := discoveredEndpointAddr[0]
			endpoints.endpoints[addr].setResponseError(errors.New("failed info request"))
			endpointSet.Update(context.Background())

			updateTime = updateTime.Add(10 * time.Minute)
			endpointSet.Update(context.Background())
			testutil.Equals(t, tc.expectedEndpoints, len(endpointSet.GetEndpointStatus()))
			testutil.Equals(t, tc.expectedEndpoints, len(endpointSet.GetStoreClients()))
		})
	}
}

func TestEndpointSetUpdate_AtomicEndpointAdditions(t *testing.T) {
	t.Parallel()

	numResponses := 4
	metas := makeInfoResponses(numResponses)
	metas[1].infoDelay = 2 * time.Second

	endpoints, err := startTestEndpoints(metas)
	testutil.Ok(t, err)
	defer endpoints.Close()

	updateTime := time.Now()
	discoveredEndpointAddr := endpoints.EndpointAddresses()
	endpointSet := makeEndpointSet(discoveredEndpointAddr, false, func() time.Time { return updateTime })
	endpointSet.endpointInfoTimeout = 3 * time.Second
	defer endpointSet.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.Never(t, func() bool {
			numStatuses := len(endpointSet.GetStoreClients())
			return numStatuses != numResponses && numStatuses != 0
		}, 3*time.Second, 100*time.Millisecond)
	}()

	endpointSet.Update(context.Background())
	testutil.Equals(t, numResponses, len(endpointSet.GetEndpointStatus()))
	testutil.Equals(t, numResponses, len(endpointSet.GetStoreClients()))
	wg.Wait()
}

func TestEndpointSetUpdate_AvailabilityScenarios(t *testing.T) {
	t.Parallel()

	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "addr", Value: addr},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "a", Value: "b"},
						},
					},
				}
			},
		},
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "addr", Value: addr},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "a", Value: "b"},
						},
					},
				}
			},
		},
		{
			InfoResponse: queryInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "addr", Value: addr},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "a", Value: "b"},
						},
					},
				}
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoints.Close()

	discoveredEndpointAddr := endpoints.EndpointAddresses()

	now := time.Now()
	nowFunc := func() time.Time { return now }
	// Testing if duplicates can cause weird results.
	discoveredEndpointAddr = append(discoveredEndpointAddr, discoveredEndpointAddr[0])
	endpointSet := NewEndpointSet(nowFunc, nil, nil,
		func() (specs []*GRPCEndpointSpec) {
			for _, addr := range discoveredEndpointAddr {
				specs = append(specs, NewGRPCEndpointSpec(addr, false, testGRPCOpts...))
			}
			return specs
		},
		time.Minute, 2*time.Second)
	defer endpointSet.Close()

	// Initial update.
	endpointSet.Update(context.Background())
	testutil.Equals(t, 3, len(endpointSet.endpoints))

	// Start with one not available.
	endpoints.CloseOne(discoveredEndpointAddr[2])

	// Should not matter how many of these we run.
	endpointSet.Update(context.Background())
	endpointSet.Update(context.Background())
	testutil.Equals(t, 2, len(endpointSet.GetStoreClients()))
	testutil.Equals(t, 3, len(endpointSet.GetEndpointStatus()))

	for addr, e := range endpointSet.endpoints {
		testutil.Equals(t, addr, e.addr)

		lset := e.LabelSets()
		testutil.Equals(t, 2, len(lset))
		testutil.Equals(t, addr, lset[0].Get("addr"))
		testutil.Equals(t, "b", lset[1].Get("a"))
		assertRegisteredAPIs(t, endpoints.exposedAPIs[addr], e)
	}

	// Check stats.
	expected := newEndpointAPIStats()
	expected[component.Sidecar.String()] = map[string]int{
		fmt.Sprintf("{a=\"b\"},{addr=\"%s\"}", discoveredEndpointAddr[0]): 1,
		fmt.Sprintf("{a=\"b\"},{addr=\"%s\"}", discoveredEndpointAddr[1]): 1,
	}
	testutil.Equals(t, expected, endpointSet.endpointsMetric.storeNodes)

	// Remove address from discovered and reset last check, which should ensure cleanup of status on next update.
	now = now.Add(3 * time.Minute)
	discoveredEndpointAddr = discoveredEndpointAddr[:len(discoveredEndpointAddr)-2]
	endpointSet.Update(context.Background())
	testutil.Equals(t, 2, len(endpointSet.endpoints))

	endpoints.CloseOne(discoveredEndpointAddr[0])
	delete(expected[component.Sidecar.String()], fmt.Sprintf("{a=\"b\"},{addr=\"%s\"}", discoveredEndpointAddr[0]))

	// We expect Update to tear down store client for closed store server.
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1, len(endpointSet.GetStoreClients()), "only one service should respond just fine, so we expect one client to be ready.")

	addr := discoveredEndpointAddr[1]
	st, ok := endpointSet.endpoints[addr]
	testutil.Assert(t, ok, "addr exist")
	testutil.Equals(t, addr, st.addr)

	lset := st.LabelSets()
	testutil.Equals(t, 2, len(lset))
	testutil.Equals(t, addr, lset[0].Get("addr"))
	testutil.Equals(t, "b", lset[1].Get("a"))
	testutil.Equals(t, expected, endpointSet.endpointsMetric.storeNodes)

	// New big batch of endpoints.
	endpoint2, err := startTestEndpoints([]testEndpointMeta{
		{
			InfoResponse: queryInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "l3", Value: "v4"},
						},
					},
				}
			},
		},
		{
			// Duplicated Querier, in previous versions it would be deduplicated. Now it should be not.
			InfoResponse: queryInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "l3", Value: "v4"},
						},
					},
				}
			},
		},
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
				}
			},
		},
		{
			// Duplicated Sidecar, in previous versions it would be deduplicated. Now it should be not.
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
				}
			},
		},
		{
			// Querier that duplicates with sidecar, in previous versions it would be deduplicated. Now it should be not.
			InfoResponse: queryInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
				}
			},
		},
		{
			// Ruler that duplicates with sidecar, in previous versions it would be deduplicated. Now it should be not.
			// Warning should be produced.
			InfoResponse: ruleInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
				}
			},
		},
		{
			// Duplicated Rule, in previous versions it would be deduplicated. Now it should be not. Warning should be produced.
			InfoResponse: ruleInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
				}
			},
		},
		// Two pre v0.8.0 store gateway nodes, they don't have ext labels set.
		{
			InfoResponse: storeGWInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{}
			},
		},
		{
			InfoResponse: storeGWInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{}
			},
		},
		// Regression tests against https://github.com/thanos-io/thanos/issues/1632: From v0.8.0 stores advertise labels.
		// If the object storage handled by store gateway has only one sidecar we used to hitting issue.
		{
			InfoResponse: storeGWInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "l3", Value: "v4"},
						},
					},
				}
			},
		},
		// Stores v0.8.1 has compatibility labels. Check if they are correctly removed.
		{
			InfoResponse: storeGWInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "l3", Value: "v4"},
						},
					},
				}
			},
		},
		// Duplicated store, in previous versions it would be deduplicated. Now it should be not.
		{
			InfoResponse: storeGWInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "l3", Value: "v4"},
						},
					},
				}
			},
		},
		{
			InfoResponse: receiveInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "l3", Value: "v4"},
						},
					},
				}
			},
		},
		// Duplicate receiver
		{
			InfoResponse: receiveInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{Name: "l1", Value: "v2"},
							{Name: "l2", Value: "v3"},
						},
					},
					{
						Labels: []labelpb.ZLabel{
							{Name: "l3", Value: "v4"},
						},
					},
				}
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoint2.Close()

	discoveredEndpointAddr = append(discoveredEndpointAddr, endpoint2.EndpointAddresses()...)

	// New stores should be loaded.
	endpointSet.Update(context.Background())
	testutil.Equals(t, 1+len(endpoint2.srvs), len(endpointSet.GetStoreClients()))

	// Check stats.
	expected = newEndpointAPIStats()
	expected[component.Query.String()] = map[string]int{
		"{l1=\"v2\", l2=\"v3\"}":             1,
		"{l1=\"v2\", l2=\"v3\"},{l3=\"v4\"}": 2,
	}
	expected[component.Rule.String()] = map[string]int{
		"{l1=\"v2\", l2=\"v3\"}": 2,
	}
	expected[component.Sidecar.String()] = map[string]int{
		fmt.Sprintf("{a=\"b\"},{addr=\"%s\"}", discoveredEndpointAddr[1]): 1,
		"{l1=\"v2\", l2=\"v3\"}": 2,
	}
	expected[component.Store.String()] = map[string]int{
		"":                                   2,
		"{l1=\"v2\", l2=\"v3\"},{l3=\"v4\"}": 3,
	}
	expected[component.Receive.String()] = map[string]int{
		"{l1=\"v2\", l2=\"v3\"},{l3=\"v4\"}": 2,
	}
	testutil.Equals(t, expected, endpointSet.endpointsMetric.storeNodes)

	// Close remaining endpoint from previous batch
	endpoints.CloseOne(discoveredEndpointAddr[1])
	endpointSet.Update(context.Background())

	for addr, e := range endpointSet.getQueryableRefs() {
		testutil.Equals(t, addr, e.addr)
		assertRegisteredAPIs(t, endpoint2.exposedAPIs[addr], e)
	}

	// Check statuses.
	testutil.Equals(t, 2+len(endpoint2.srvs), len(endpointSet.GetEndpointStatus()))
}

func TestEndpointSet_Update_NoneAvailable(t *testing.T) {
	t.Parallel()

	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{
								Name:  "addr",
								Value: addr,
							},
						},
					},
				}
			},
		},
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{
								Name:  "addr",
								Value: addr,
							},
						},
					},
				}
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoints.Close()

	initialEndpointAddr := endpoints.EndpointAddresses()
	endpoints.CloseOne(initialEndpointAddr[0])
	endpoints.CloseOne(initialEndpointAddr[1])

	endpointSet := NewEndpointSet(time.Now, nil, nil,
		func() (specs []*GRPCEndpointSpec) {
			for _, addr := range initialEndpointAddr {
				specs = append(specs, NewGRPCEndpointSpec(addr, false))
			}
			return specs
		},
		time.Minute, 2*time.Second)
	defer endpointSet.Close()

	// Should not matter how many of these we run.
	endpointSet.Update(context.Background())
	endpointSet.Update(context.Background())
	testutil.Equals(t, 0, len(endpointSet.GetStoreClients()), "none of services should respond just fine, so we expect no client to be ready.")

	// Leak test will ensure that we don't keep client connection around.
	expected := newEndpointAPIStats()
	testutil.Equals(t, expected, endpointSet.endpointsMetric.storeNodes)

}

// TestEndpoint_Update_QuerierStrict tests what happens when the strict mode is enabled/disabled.
func TestEndpoint_Update_QuerierStrict(t *testing.T) {
	t.Parallel()

	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			InfoResponse: &infopb.InfoResponse{
				ComponentType: component.Sidecar.String(),
				Store: &infopb.StoreInfo{
					MinTime: 12345,
					MaxTime: 54321,
				},
				Exemplars:      &infopb.ExemplarsInfo{},
				Rules:          &infopb.RulesInfo{},
				MetricMetadata: &infopb.MetricMetadataInfo{},
				Targets:        &infopb.TargetsInfo{},
			},
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{
								Name:  "addr",
								Value: addr,
							},
						},
					},
				}
			},
		},
		{
			InfoResponse: &infopb.InfoResponse{
				ComponentType: component.Sidecar.String(),
				Store: &infopb.StoreInfo{
					MinTime: 66666,
					MaxTime: 77777,
				},
				Exemplars:      &infopb.ExemplarsInfo{},
				Rules:          &infopb.RulesInfo{},
				MetricMetadata: &infopb.MetricMetadataInfo{},
				Targets:        &infopb.TargetsInfo{},
			},
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{
								Name:  "addr",
								Value: addr,
							},
						},
					},
				}
			},
		},
		// Slow store.
		{
			InfoResponse: &infopb.InfoResponse{
				ComponentType: component.Sidecar.String(),
				Store: &infopb.StoreInfo{
					MinTime: 65644,
					MaxTime: 77777,
				},
				Exemplars:      &infopb.ExemplarsInfo{},
				Rules:          &infopb.RulesInfo{},
				MetricMetadata: &infopb.MetricMetadataInfo{},
				Targets:        &infopb.TargetsInfo{},
			},
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{
					{
						Labels: []labelpb.ZLabel{
							{
								Name:  "addr",
								Value: addr,
							},
						},
					},
				}
			},
			infoDelay: 2 * time.Second,
		},
	})

	testutil.Ok(t, err)
	defer endpoints.Close()

	discoveredEndpointAddr := endpoints.EndpointAddresses()

	staticEndpointAddr := discoveredEndpointAddr[0]
	slowStaticEndpointAddr := discoveredEndpointAddr[2]
	endpointSet := NewEndpointSet(time.Now, nil, nil, func() (specs []*GRPCEndpointSpec) {
		return []*GRPCEndpointSpec{
			NewGRPCEndpointSpec(discoveredEndpointAddr[0], true, testGRPCOpts...),
			NewGRPCEndpointSpec(discoveredEndpointAddr[1], false, testGRPCOpts...),
			NewGRPCEndpointSpec(discoveredEndpointAddr[2], true, testGRPCOpts...),
		}
	}, time.Minute, 1*time.Second)
	defer endpointSet.Close()

	// Initial update.
	endpointSet.Update(context.Background())
	testutil.Equals(t, 3, len(endpointSet.endpoints), "three clients must be available for running nodes")

	// The endpoint has not responded to the info call and is assumed to cover everything.
	curMin, curMax := endpointSet.endpoints[slowStaticEndpointAddr].metadata.Store.MinTime, endpointSet.endpoints[slowStaticEndpointAddr].metadata.Store.MaxTime
	testutil.Assert(t, endpointSet.endpoints[slowStaticEndpointAddr].cc.GetState().String() != "SHUTDOWN", "slow store's connection should not be closed")
	testutil.Equals(t, int64(math.MinInt64), curMin)
	testutil.Equals(t, int64(math.MaxInt64), curMax)

	// The endpoint is statically defined + strict mode is enabled
	// so its client + information must be retained.
	curMin, curMax = endpointSet.endpoints[staticEndpointAddr].metadata.Store.MinTime, endpointSet.endpoints[staticEndpointAddr].metadata.Store.MaxTime
	testutil.Equals(t, int64(12345), curMin, "got incorrect minimum time")
	testutil.Equals(t, int64(54321), curMax, "got incorrect minimum time")

	// Successfully retrieve the information and observe minTime/maxTime updating.
	endpointSet.endpointInfoTimeout = 3 * time.Second
	endpointSet.Update(context.Background())
	updatedCurMin, updatedCurMax := endpointSet.endpoints[slowStaticEndpointAddr].metadata.Store.MinTime, endpointSet.endpoints[slowStaticEndpointAddr].metadata.Store.MaxTime
	testutil.Equals(t, int64(65644), updatedCurMin)
	testutil.Equals(t, int64(77777), updatedCurMax)
	endpointSet.endpointInfoTimeout = 1 * time.Second

	// Turn off the endpoints.
	endpoints.Close()

	// Update again many times. Should not matter WRT the static one.
	endpointSet.Update(context.Background())
	endpointSet.Update(context.Background())
	endpointSet.Update(context.Background())

	// Check that the information is the same.
	testutil.Equals(t, 2, len(endpointSet.GetStoreClients()), "two static clients must remain available")
	testutil.Equals(t, curMin, endpointSet.endpoints[staticEndpointAddr].metadata.Store.MinTime, "minimum time reported by the store node is different")
	testutil.Equals(t, curMax, endpointSet.endpoints[staticEndpointAddr].metadata.Store.MaxTime, "minimum time reported by the store node is different")
	testutil.NotOk(t, endpointSet.endpoints[staticEndpointAddr].status.LastError.originalErr)

	testutil.Equals(t, updatedCurMin, endpointSet.endpoints[slowStaticEndpointAddr].metadata.Store.MinTime, "minimum time reported by the store node is different")
	testutil.Equals(t, updatedCurMax, endpointSet.endpoints[slowStaticEndpointAddr].metadata.Store.MaxTime, "minimum time reported by the store node is different")
}

func TestEndpointSet_APIs_Discovery(t *testing.T) {
	t.Parallel()

	endpoints, err := startTestEndpoints([]testEndpointMeta{
		{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{}
			},
		},
		{
			InfoResponse: ruleInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{}
			},
		},
		{
			InfoResponse: receiveInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{}
			},
		},
		{
			InfoResponse: storeGWInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{}
			},
		},
		{
			InfoResponse: queryInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return []labelpb.ZLabelSet{}
			},
		},
	})
	testutil.Ok(t, err)
	defer endpoints.Close()

	type discoveryState struct {
		name                   string
		endpointSpec           func() []*GRPCEndpointSpec
		expectedStores         int
		expectedRules          int
		expectedTarget         int
		expectedMetricMetadata int
		expectedExemplars      int
		expectedQueryAPIs      int
	}

	for _, tc := range []struct {
		states []discoveryState
		name   string
	}{
		{
			name: "All endpoints discovered concurrently",
			states: []discoveryState{
				{
					name:         "no endpoints",
					endpointSpec: nil,
				},
				{
					name: "Sidecar, Ruler, Querier, Receiver and StoreGW discovered",
					endpointSpec: func() []*GRPCEndpointSpec {
						endpointSpec := make([]*GRPCEndpointSpec, 0, len(endpoints.orderAddrs))
						for _, addr := range endpoints.orderAddrs {
							endpointSpec = append(endpointSpec, NewGRPCEndpointSpec(addr, false, testGRPCOpts...))
						}
						return endpointSpec
					},
					expectedStores:         5, // sidecar + querier + receiver + storeGW + ruler
					expectedRules:          3, // sidecar + querier + ruler
					expectedTarget:         2, // sidecar + querier
					expectedMetricMetadata: 2, // sidecar + querier
					expectedExemplars:      3, // sidecar + querier + receiver
					expectedQueryAPIs:      1, // querier
				},
			},
		},
		{
			name: "Sidecar discovery first, eventually Ruler discovered and then Sidecar removed",
			states: []discoveryState{
				{
					name:         "no stores",
					endpointSpec: nil,
				},
				{
					name: "Sidecar discovered, no Ruler discovered",
					endpointSpec: func() []*GRPCEndpointSpec {
						return []*GRPCEndpointSpec{
							NewGRPCEndpointSpec(endpoints.orderAddrs[0], false, testGRPCOpts...),
						}
					},
					expectedStores:         1, // sidecar
					expectedRules:          1, // sidecar
					expectedTarget:         1, // sidecar
					expectedMetricMetadata: 1, // sidecar
					expectedExemplars:      1, // sidecar
				},
				{
					name: "Ruler discovered",
					endpointSpec: func() []*GRPCEndpointSpec {
						return []*GRPCEndpointSpec{
							NewGRPCEndpointSpec(endpoints.orderAddrs[0], false, testGRPCOpts...),
							NewGRPCEndpointSpec(endpoints.orderAddrs[1], false, testGRPCOpts...),
						}
					},
					expectedStores:         2, // sidecar + ruler
					expectedRules:          2, // sidecar + ruler
					expectedTarget:         1, // sidecar
					expectedMetricMetadata: 1, // sidecar
					expectedExemplars:      1, // sidecar
				},
				{
					name: "Sidecar removed",
					endpointSpec: func() []*GRPCEndpointSpec {
						return []*GRPCEndpointSpec{
							NewGRPCEndpointSpec(endpoints.orderAddrs[1], false, testGRPCOpts...),
						}
					},
					expectedStores: 1, // ruler
					expectedRules:  1, // ruler
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentState := 0

			endpointSet := NewEndpointSet(time.Now, nil, nil,
				func() []*GRPCEndpointSpec {
					if tc.states[currentState].endpointSpec == nil {
						return nil
					}

					return tc.states[currentState].endpointSpec()
				},
				time.Minute, 2*time.Second)

			defer endpointSet.Close()

			for {
				endpointSet.Update(context.Background())

				gotStores := 0
				gotRules := 0
				gotTarget := 0
				gotExemplars := 0
				gotMetricMetadata := 0
				gotQueryAPIs := 0

				for _, er := range endpointSet.endpoints {
					if er.HasStoreAPI() {
						gotStores += 1
					}
					if er.HasRulesAPI() {
						gotRules += 1
					}
					if er.HasTargetsAPI() {
						gotTarget += 1
					}
					if er.HasExemplarsAPI() {
						gotExemplars += 1
					}
					if er.HasMetricMetadataAPI() {
						gotMetricMetadata += 1
					}
					if er.HasQueryAPI() {
						gotQueryAPIs += 1
					}
				}
				testutil.Equals(
					t,
					tc.states[currentState].expectedStores,
					gotStores,
					"unexpected discovered storeAPIs in state %q",
					tc.states[currentState].name)
				testutil.Equals(
					t,
					tc.states[currentState].expectedRules,
					gotRules,
					"unexpected discovered rulesAPIs in state %q",
					tc.states[currentState].name)
				testutil.Equals(
					t,
					tc.states[currentState].expectedTarget,
					gotTarget,
					"unexpected discovered targetAPIs in state %q",
					tc.states[currentState].name,
				)
				testutil.Equals(
					t,
					tc.states[currentState].expectedMetricMetadata,
					gotMetricMetadata,
					"unexpected discovered metricMetadataAPIs in state %q",
					tc.states[currentState].name,
				)
				testutil.Equals(
					t,
					tc.states[currentState].expectedExemplars,
					gotExemplars,
					"unexpected discovered ExemplarsAPIs in state %q",
					tc.states[currentState].name,
				)
				testutil.Equals(
					t,
					tc.states[currentState].expectedQueryAPIs,
					gotQueryAPIs,
					"unexpected discovered QueryAPIs in state %q",
					tc.states[currentState].name,
				)

				currentState = currentState + 1
				if len(tc.states) == currentState {
					break
				}
			}
		})
	}
}

func makeInfoResponses(n int) []testEndpointMeta {
	responses := make([]testEndpointMeta, 0, n)
	for i := 0; i < n; i++ {
		responses = append(responses, testEndpointMeta{
			InfoResponse: sidecarInfo,
			extlsetFn: func(addr string) []labelpb.ZLabelSet {
				return labelpb.ZLabelSetsFromPromLabels(
					labels.FromStrings("addr", addr, "a", "b"),
				)
			},
		})
	}

	return responses
}

type errThatMarshalsToEmptyDict struct {
	msg string
}

// MarshalJSON marshals the error and returns and empty dict, not the error string.
func (e *errThatMarshalsToEmptyDict) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{})
}

// Error returns the original, underlying string.
func (e *errThatMarshalsToEmptyDict) Error() string {
	return e.msg
}

// Test highlights that without wrapping the error, it is marshaled to empty dict {}, not its message.
func TestEndpointStringError(t *testing.T) {
	t.Parallel()

	dictErr := &errThatMarshalsToEmptyDict{msg: "Error message"}
	stringErr := &stringError{originalErr: dictErr}

	endpointstatusMock := map[string]error{}
	endpointstatusMock["dictErr"] = dictErr
	endpointstatusMock["stringErr"] = stringErr

	b, err := json.Marshal(endpointstatusMock)

	testutil.Ok(t, err)
	testutil.Equals(t, []byte(`{"dictErr":{},"stringErr":"Error message"}`), b, "expected to get proper results")
}

// Errors that usually marshal to empty dict should return the original error string.
func TestUpdateEndpointStateLastError(t *testing.T) {
	t.Parallel()

	tcs := []struct {
		InputError      error
		ExpectedLastErr string
	}{
		{errors.New("normal_err"), `"normal_err"`},
		{nil, `null`},
		{&errThatMarshalsToEmptyDict{"the error message"}, `"the error message"`},
	}

	for _, tc := range tcs {
		mockEndpointRef := &endpointRef{
			addr: "mockedStore",
			metadata: &endpointMetadata{
				&infopb.InfoResponse{},
			},
		}

		mockEndpointRef.update(time.Now, mockEndpointRef.metadata, tc.InputError)

		b, err := json.Marshal(mockEndpointRef.status.LastError)
		testutil.Ok(t, err)
		testutil.Equals(t, tc.ExpectedLastErr, string(b))
	}
}

func TestUpdateEndpointStateForgetsPreviousErrors(t *testing.T) {
	t.Parallel()

	mockEndpointRef := &endpointRef{
		addr: "mockedStore",
		metadata: &endpointMetadata{
			&infopb.InfoResponse{},
		},
	}

	mockEndpointRef.update(time.Now, mockEndpointRef.metadata, errors.New("test err"))

	b, err := json.Marshal(mockEndpointRef.status.LastError)
	testutil.Ok(t, err)
	testutil.Equals(t, `"test err"`, string(b))

	// updating status without and error should clear the previous one.
	mockEndpointRef.update(time.Now, mockEndpointRef.metadata, nil)

	b, err = json.Marshal(mockEndpointRef.status.LastError)
	testutil.Ok(t, err)
	testutil.Equals(t, `null`, string(b))
}

func makeEndpointSet(discoveredEndpointAddr []string, strict bool, now nowFunc, metricLabels ...string) *EndpointSet {
	endpointSet := NewEndpointSet(now, nil, nil,
		func() (specs []*GRPCEndpointSpec) {
			for _, addr := range discoveredEndpointAddr {
				specs = append(specs, NewGRPCEndpointSpec(addr, strict, testGRPCOpts...))
			}
			return specs
		},
		time.Minute, time.Second, metricLabels...)
	return endpointSet
}

func exposedAPIs(c string) *APIs {
	switch c {
	case component.Sidecar.String():
		return &APIs{
			store:          true,
			target:         true,
			rules:          true,
			metricMetadata: true,
			exemplars:      true,
		}
	case component.Query.String():
		return &APIs{
			store:          true,
			target:         true,
			rules:          true,
			metricMetadata: true,
			exemplars:      true,
		}
	case component.Receive.String():
		return &APIs{
			store:     true,
			exemplars: true,
		}
	case component.Rule.String():
		return &APIs{
			store: true,
			rules: true,
		}
	case component.Store.String():
		return &APIs{
			store: true,
		}
	}
	return &APIs{}
}

func assertRegisteredAPIs(t *testing.T, expectedAPIs *APIs, er *endpointRef) {
	testutil.Equals(t, expectedAPIs.store, er.HasStoreAPI())
	testutil.Equals(t, expectedAPIs.rules, er.HasRulesAPI())
	testutil.Equals(t, expectedAPIs.target, er.HasTargetsAPI())
	testutil.Equals(t, expectedAPIs.metricMetadata, er.HasMetricMetadataAPI())
	testutil.Equals(t, expectedAPIs.exemplars, er.HasExemplarsAPI())
}

// Regression test for: https://github.com/thanos-io/thanos/issues/4766.
func TestDeadlockLocking(t *testing.T) {
	t.Parallel()

	mockEndpointRef := &endpointRef{
		addr: "mockedStore",
		metadata: &endpointMetadata{
			&infopb.InfoResponse{},
		},
	}

	g := &errgroup.Group{}
	deadline := time.Now().Add(3 * time.Second)

	g.Go(func() error {
		for {
			if time.Now().After(deadline) {
				break
			}
			mockEndpointRef.update(time.Now, &endpointMetadata{
				InfoResponse: &infopb.InfoResponse{},
			}, nil)
		}
		return nil
	})

	g.Go(func() error {
		for {
			if time.Now().After(deadline) {
				break
			}
			mockEndpointRef.HasStoreAPI()
			mockEndpointRef.HasExemplarsAPI()
			mockEndpointRef.HasMetricMetadataAPI()
			mockEndpointRef.HasRulesAPI()
			mockEndpointRef.HasTargetsAPI()
		}
		return nil
	})

	testutil.Ok(t, g.Wait())
}

func TestEndpointSet_WaitForFirstUpdate(t *testing.T) {
	t.Parallel()

	t.Run("WaitForFirstUpdate blocks until first update", func(t *testing.T) {
		endpoints, err := startTestEndpoints([]testEndpointMeta{
			{
				InfoResponse: sidecarInfo,
				extlsetFn: func(addr string) []labelpb.ZLabelSet {
					return labelpb.ZLabelSetsFromPromLabels(
						labels.FromStrings("addr", addr),
					)
				},
			},
		})
		testutil.Ok(t, err)
		defer endpoints.Close()

		discoveredEndpointAddr := endpoints.EndpointAddresses()
		endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
		defer endpointSet.Close()

		// Track when WaitForFirstUpdate returns
		waitReturned := make(chan struct{})
		var waitErr error

		// Start waiting in a goroutine
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			waitErr = endpointSet.WaitForFirstUpdate(ctx)
			close(waitReturned)
		}()

		// Give some time to ensure WaitForFirstUpdate is blocking
		select {
		case <-waitReturned:
			t.Fatal("WaitForFirstUpdate returned before Update was called")
		case <-time.After(100 * time.Millisecond):
			// Expected: still waiting
		}

		// Now trigger the update
		endpointSet.Update(context.Background())

		// WaitForFirstUpdate should return quickly after Update
		select {
		case <-waitReturned:
			testutil.Ok(t, waitErr)
		case <-time.After(1 * time.Second):
			t.Fatal("WaitForFirstUpdate did not return after Update")
		}

		// Verify endpoints were discovered
		testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
		testutil.Equals(t, 1, len(endpointSet.GetStoreClients()))
	})

	t.Run("WaitForFirstUpdate returns immediately if update already done", func(t *testing.T) {
		endpoints, err := startTestEndpoints([]testEndpointMeta{
			{
				InfoResponse: sidecarInfo,
				extlsetFn: func(addr string) []labelpb.ZLabelSet {
					return labelpb.ZLabelSetsFromPromLabels(
						labels.FromStrings("addr", addr),
					)
				},
			},
		})
		testutil.Ok(t, err)
		defer endpoints.Close()

		discoveredEndpointAddr := endpoints.EndpointAddresses()
		endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
		defer endpointSet.Close()

		// First do an update
		endpointSet.Update(context.Background())

		// Now WaitForFirstUpdate should return immediately
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		err = endpointSet.WaitForFirstUpdate(ctx)
		duration := time.Since(start)

		testutil.Ok(t, err)
		testutil.Assert(t, duration < 50*time.Millisecond, "WaitForFirstUpdate took too long: %v", duration)
	})

	t.Run("WaitForFirstUpdate respects context timeout", func(t *testing.T) {
		endpoints, err := startTestEndpoints([]testEndpointMeta{
			{
				InfoResponse: sidecarInfo,
				extlsetFn: func(addr string) []labelpb.ZLabelSet {
					return labelpb.ZLabelSetsFromPromLabels(
						labels.FromStrings("addr", addr),
					)
				},
			},
		})
		testutil.Ok(t, err)
		defer endpoints.Close()

		discoveredEndpointAddr := endpoints.EndpointAddresses()
		endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
		defer endpointSet.Close()

		// Use a short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		err = endpointSet.WaitForFirstUpdate(ctx)
		duration := time.Since(start)

		// Should timeout
		testutil.NotOk(t, err)
		testutil.Assert(t, errors.Is(err, context.DeadlineExceeded), "expected context.DeadlineExceeded, got %v", err)
		testutil.Assert(t, duration >= 100*time.Millisecond && duration < 200*time.Millisecond,
			"timeout duration out of expected range: %v", duration)
	})

	t.Run("Multiple WaitForFirstUpdate calls work correctly", func(t *testing.T) {
		endpoints, err := startTestEndpoints([]testEndpointMeta{
			{
				InfoResponse: sidecarInfo,
				extlsetFn: func(addr string) []labelpb.ZLabelSet {
					return labelpb.ZLabelSetsFromPromLabels(
						labels.FromStrings("addr", addr),
					)
				},
			},
		})
		testutil.Ok(t, err)
		defer endpoints.Close()

		discoveredEndpointAddr := endpoints.EndpointAddresses()
		endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
		defer endpointSet.Close()

		// Start multiple waiters
		var wg sync.WaitGroup
		errors := make([]error, 3)

		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				errors[idx] = endpointSet.WaitForFirstUpdate(ctx)
			}(i)
		}

		// Give waiters time to start
		time.Sleep(100 * time.Millisecond)

		// Trigger update
		endpointSet.Update(context.Background())

		// All waiters should complete
		wg.Wait()

		// All should succeed
		for i, err := range errors {
			testutil.Ok(t, err, "waiter %d failed", i)
		}

		// Additional calls should still work
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err = endpointSet.WaitForFirstUpdate(ctx)
		testutil.Ok(t, err)
	})

	t.Run("Update called multiple times only signals once", func(t *testing.T) {
		endpoints, err := startTestEndpoints([]testEndpointMeta{
			{
				InfoResponse: sidecarInfo,
				extlsetFn: func(addr string) []labelpb.ZLabelSet {
					return labelpb.ZLabelSetsFromPromLabels(
						labels.FromStrings("addr", addr),
					)
				},
			},
		})
		testutil.Ok(t, err)
		defer endpoints.Close()

		discoveredEndpointAddr := endpoints.EndpointAddresses()
		endpointSet := makeEndpointSet(discoveredEndpointAddr, false, time.Now)
		defer endpointSet.Close()

		// Call Update multiple times
		for i := 0; i < 3; i++ {
			endpointSet.Update(context.Background())
		}

		// WaitForFirstUpdate should still work correctly
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err = endpointSet.WaitForFirstUpdate(ctx)
		testutil.Ok(t, err)

		// Verify only one endpoint set (no duplicates from multiple updates)
		testutil.Equals(t, 1, len(endpointSet.GetEndpointStatus()))
		testutil.Equals(t, 1, len(endpointSet.GetStoreClients()))
	})
}
