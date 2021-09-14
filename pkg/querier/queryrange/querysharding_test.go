// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/querysharding_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package queryrange

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/pkg/value"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/grafana/mimir/pkg/util"
)

func mockHandlerWith(resp *PrometheusResponse, err error) Handler {
	return HandlerFunc(func(ctx context.Context, req Request) (Response, error) {
		if expired := ctx.Err(); expired != nil {
			return nil, expired
		}

		return resp, err
	})
}

// approximatelyEquals ensures two responses are approximately equal, up to 6 decimals precision per sample
func approximatelyEquals(t *testing.T, a, b *PrometheusResponse) {
	// Ensure both queries succeeded.
	require.Equal(t, StatusSuccess, a.Status)
	require.Equal(t, StatusSuccess, b.Status)

	as, err := ResponseToSamples(a)
	require.Nil(t, err)
	bs, err := ResponseToSamples(b)
	require.Nil(t, err)

	require.Equal(t, len(as), len(bs), "expected same number of series")

	for i := 0; i < len(as); i++ {
		a := as[i]
		b := bs[i]
		require.Equal(t, a.Labels, b.Labels)
		require.Equal(t, len(a.Samples), len(b.Samples), "expected same number of samples for series %s", a.Labels)

		for j := 0; j < len(a.Samples); j++ {
			expected := a.Samples[j]
			actual := b.Samples[j]
			require.Equalf(t, expected.TimestampMs, actual.TimestampMs, "sample timestamp at position %d for series %s", j, a.Labels)

			if value.IsStaleNaN(expected.Value) {
				require.Truef(t, value.IsStaleNaN(actual.Value), "sample value at position %d is expected to be stale marker for series %s", j, a.Labels)
			} else if math.IsNaN(expected.Value) {
				require.Truef(t, math.IsNaN(actual.Value), "sample value at position %d is expected to be NaN for series %s", j, a.Labels)
			} else {
				if expected.Value == 0 {
					require.Zero(t, actual.Value, "sample value at position %d with timestamp %d for series %s", j, expected.TimestampMs, a.Labels)
					continue
				}
				// InEpsilon means the relative error (see https://en.wikipedia.org/wiki/Relative_error#Example) must be less than epsilon (here 1e-12).
				// The relative error is calculated using: abs(actual-expected) / abs(expected)
				require.InEpsilonf(t, expected.Value, actual.Value, 1e-12, "sample value at position %d with timestamp %d for series %s", j, expected.TimestampMs, a.Labels)
			}
		}
	}
}

func TestQueryShardingCorrectness(t *testing.T) {
	var (
		numSeries          = 1000
		numStaleSeries     = 100
		numHistograms      = 1000
		numStaleHistograms = 100
		histogramBuckets   = []float64{1.0, 2.0, 4.0, 10.0, 100.0, math.Inf(1)}
	)

	tests := map[string]struct {
		query string

		// Expected number of sharded queries per shard (the final expected
		// number will be multiplied for the number of shards).
		expectedShardedQueries int
	}{
		"sum() no grouping": {
			query:                  `sum(metric_counter)`,
			expectedShardedQueries: 1,
		},
		"sum() grouping 'by'": {
			query:                  `sum by(group_1) (metric_counter)`,
			expectedShardedQueries: 1,
		},
		"sum() grouping 'without'": {
			query:                  `sum without(unique) (metric_counter)`,
			expectedShardedQueries: 1,
		},
		"sum(rate()) no grouping": {
			query:                  `sum(rate(metric_counter[1m]))`,
			expectedShardedQueries: 1,
		},
		"sum(rate()) grouping 'by'": {
			query:                  `sum by(group_1) (rate(metric_counter[1m]))`,
			expectedShardedQueries: 1,
		},
		"sum(rate()) grouping 'without'": {
			query:                  `sum without(unique) (rate(metric_counter[1m]))`,
			expectedShardedQueries: 1,
		},
		"sum(rate()) with no effective grouping because all groups have 1 series": {
			query:                  `sum by(unique) (rate(metric_counter[1m]))`,
			expectedShardedQueries: 1,
		},
		"histogram_quantile() grouping only 'by' le": {
			query:                  `histogram_quantile(0.5, sum by(le) (rate(metric_histogram_bucket[1m])))`,
			expectedShardedQueries: 1,
		},
		"histogram_quantile() grouping 'by'": {
			query:                  `histogram_quantile(0.5, sum by(group_1, le) (rate(metric_histogram_bucket[1m])))`,
			expectedShardedQueries: 1,
		},
		"histogram_quantile() grouping 'without'": {
			query:                  `histogram_quantile(0.5, sum without(group_1, group_2, unique) (rate(metric_histogram_bucket[1m])))`,
			expectedShardedQueries: 1,
		},
		"histogram_quantile() with no effective grouping because all groups have 1 series": {
			query:                  `histogram_quantile(0.5, sum by(unique, le) (rate(metric_histogram_bucket[1m])))`,
			expectedShardedQueries: 1,
		},
		"histogram_quantile with inner aggregation": {
			query:                  `sum by (group_1) (histogram_quantile(0.9, rate(metric_histogram_bucket[1m])))`,
			expectedShardedQueries: 1,
		},
		"histogram_quantile without aggregation": {
			query:                  `histogram_quantile(0.5, rate(metric_histogram_bucket[1m]))`,
			expectedShardedQueries: 1,
		},
		"min() no grouping": {
			query:                  `min(metric_counter{group_1="0"})`,
			expectedShardedQueries: 1,
		},
		"min() grouping 'by'": {
			query:                  `min by(group_2) (metric_counter{group_1="0"})`,
			expectedShardedQueries: 1,
		},
		"min() grouping 'without'": {
			query:                  `min without(unique) (metric_counter{group_1="0"})`,
			expectedShardedQueries: 1,
		},
		"max() no grouping": {
			query:                  `max(metric_counter{group_1="0"})`,
			expectedShardedQueries: 1,
		},
		"max() grouping 'by'": {
			query:                  `max by(group_2) (metric_counter{group_1="0"})`,
			expectedShardedQueries: 1,
		},
		"max() grouping 'without'": {
			query:                  `max without(unique) (metric_counter{group_1="0"})`,
			expectedShardedQueries: 1,
		},
		"count() no grouping": {
			query:                  `count(metric_counter)`,
			expectedShardedQueries: 1,
		},
		"count() grouping 'by'": {
			query:                  `count by(group_2) (metric_counter)`,
			expectedShardedQueries: 1,
		},
		"count() grouping 'without'": {
			query:                  `count without(unique) (metric_counter)`,
			expectedShardedQueries: 1,
		},
		"sum(count())": {
			query:                  `sum(count by(group_1) (metric_counter))`,
			expectedShardedQueries: 1,
		},
		"avg() no grouping": {
			query:                  `avg(metric_counter)`,
			expectedShardedQueries: 2, // avg() is parallelized as sum()/count().
		},
		"avg() grouping 'by'": {
			query:                  `avg by(group_2) (metric_counter)`,
			expectedShardedQueries: 2, // avg() is parallelized as sum()/count().
		},
		"avg() grouping 'without'": {
			query:                  `avg without(unique) (metric_counter)`,
			expectedShardedQueries: 2, // avg() is parallelized as sum()/count().
		},
		"sum(min_over_time())": {
			query:                  `sum by (group_1, group_2) (min_over_time(metric_counter{const="fixed"}[2m]))`,
			expectedShardedQueries: 1,
		},
		"sum(max_over_time())": {
			query:                  `sum by (group_1, group_2) (max_over_time(metric_counter{const="fixed"}[2m]))`,
			expectedShardedQueries: 1,
		},
		"sum(avg_over_time())": {
			query:                  `sum by (group_1, group_2) (avg_over_time(metric_counter{const="fixed"}[2m]))`,
			expectedShardedQueries: 1,
		},
		"or": {
			query:                  `sum(rate(metric_counter{group_1="0"}[1m])) or sum(rate(metric_counter{group_1="1"}[1m]))`,
			expectedShardedQueries: 2,
		},
		"and": {
			query: `
				sum without(unique) (rate(metric_counter{group_1="0"}[1m]))
				and
				max without(unique) (metric_counter) > 0`,
			expectedShardedQueries: 2,
		},
		"sum(rate()) > avg(rate())": {
			query: `
				sum(rate(metric_counter[1m]))
				>
				avg(rate(metric_counter[1m]))`,
			expectedShardedQueries: 3, // avg() is parallelized as sum()/count().
		},
		"nested count()": {
			query: `sum(
				  count(
				    count(metric_counter) by (group_1, group_2)
				  ) by (group_1)
				)`,
			expectedShardedQueries: 1,
		},
		"subquery max": {
			query: `max_over_time(
							rate(metric_counter[1m])
						[5m:1m]
					)`,
			expectedShardedQueries: 1,
		},
		"subquery min": {
			query: `min_over_time(
							rate(metric_counter[1m])
						[5m:1m]
					)`,
			expectedShardedQueries: 1,
		},
		"sum of subquery min": {
			query:                  `sum by(group_1) (min_over_time((changes(metric_counter[5m]))[10m:2m]))`,
			expectedShardedQueries: 1,
		},
		"triple subquery": {
			query: `max_over_time(
						stddev_over_time(
							deriv(
								rate(metric_counter[10m])
							[5m:1m])
						[2m:])
					[10m:])`,
			expectedShardedQueries: 1,
		},
		"double subquery deriv": {
			query:                  `max_over_time( deriv( rate(metric_counter[10m])[5m:1m] )[10m:] )`,
			expectedShardedQueries: 1,
		},
		//
		// The following queries are not expected to be shardable.
		//
		"subquery min_over_time with aggr": {
			query: `min_over_time(
						sum by(group_1) (
							rate(metric_counter[5m])
						)[10m:]
					)`,
			expectedShardedQueries: 0,
		},
		"stddev()": {
			query:                  `stddev(metric_counter{const="fixed"})`,
			expectedShardedQueries: 0,
		},
		"stdvar()": {
			query:                  `stdvar(metric_counter{const="fixed"})`,
			expectedShardedQueries: 0,
		},
		"topk()": {
			query:                  `topk(2, metric_counter{const="fixed"})`,
			expectedShardedQueries: 0,
		},
		"bottomk()": {
			query:                  `bottomk(2, metric_counter{const="fixed"})`,
			expectedShardedQueries: 0,
		},
		"vector()": {
			query:                  `vector(1)`,
			expectedShardedQueries: 0,
		},
		"scalar()": {
			query:                  `scalar(metric_counter{unique="1"})`, // Select a single metric.
			expectedShardedQueries: 0,
		},
		"histogram_quantile() no grouping": {
			query:                  fmt.Sprintf(`histogram_quantile(0.99, metric_histogram_bucket{unique="%d"})`, numSeries+10), // Select a single histogram metric.
			expectedShardedQueries: 0,
		},
	}

	series := make([]*promql.StorageSeries, 0, numSeries+(numHistograms*len(histogramBuckets)))
	seriesID := 0

	// Add counter series.
	for i := 0; i < numSeries; i++ {
		gen := factor(float64(i) * 0.1)
		if i >= numSeries-numStaleSeries {
			// Wrap the generator to inject the staleness marker between minute 10 and 20.
			gen = stale(start.Add(10*time.Minute), start.Add(20*time.Minute), gen)
		}

		series = append(series, newSeries(newTestCounterLabels(seriesID), start.Add(-lookbackDelta), end, gen))
		seriesID++
	}

	// Add a special series whose data points end earlier than the end of the queried time range
	// and has NO stale marker.
	series = append(series, newSeries(newTestCounterLabels(seriesID),
		start.Add(-lookbackDelta), end.Add(-5*time.Minute), factor(2)))
	seriesID++

	// Add a special series whose data points end earlier than the end of the queried time range
	// and HAS a stale marker at the end.
	series = append(series, newSeries(newTestCounterLabels(seriesID),
		start.Add(-lookbackDelta), end.Add(-5*time.Minute), stale(end.Add(-6*time.Minute), end.Add(-4*time.Minute), factor(2))))
	seriesID++

	// Add a special series whose data points start later than the start of the queried time range.
	series = append(series, newSeries(newTestCounterLabels(seriesID),
		start.Add(5*time.Minute), end, factor(2)))
	seriesID++

	// Add histogram series.
	for i := 0; i < numHistograms; i++ {
		for bucketIdx, bucketLe := range histogramBuckets {
			// We expect each bucket to have a value higher than the previous one.
			gen := factor(float64(i) * float64(bucketIdx) * 0.1)
			if i >= numHistograms-numStaleHistograms {
				// Wrap the generator to inject the staleness marker between minute 10 and 20.
				gen = stale(start.Add(10*time.Minute), start.Add(20*time.Minute), gen)
			}

			series = append(series, newSeries(newTestHistogramLabels(seriesID, bucketLe),
				start.Add(-lookbackDelta), end, gen))
		}

		// Increase the series ID after all per-bucket series have been created.
		seriesID++
	}

	// Create a queryable on the fixtures.
	queryable := storage.QueryableFunc(func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		return &querierMock{
			series: series,
		}, nil
	})

	for testName, testData := range tests {
		for _, numShards := range []int{2, 4, 8, 16} {
			t.Run(fmt.Sprintf("%s (shards: %d)", testName, numShards), func(t *testing.T) {
				req := &PrometheusRequest{
					Path:  "/query_range",
					Start: util.TimeToMillis(start),
					End:   util.TimeToMillis(end),
					Step:  step.Milliseconds(),
					Query: testData.query,
				}

				reg := prometheus.NewPedanticRegistry()
				engine := newEngine()
				shardingware := NewQueryShardingMiddleware(
					log.NewNopLogger(),
					engine,
					numShards,
					reg,
				)
				downstream := &downstreamHandler{
					engine:    engine,
					queryable: queryable,
				}

				// Run the query without sharding.
				expectedRes, err := downstream.Do(context.Background(), req)
				require.Nil(t, err)

				// Ensure the query produces some results.
				require.NotEmpty(t, expectedRes.(*PrometheusResponse).Data.Result)

				// Ensure the query produces some results which are not NaN.
				foundValidSamples := false
			outer:
				for _, stream := range expectedRes.(*PrometheusResponse).Data.Result {
					for _, sample := range stream.Samples {
						if !math.IsNaN(sample.Value) {
							foundValidSamples = true
							break outer
						}
					}
				}
				require.True(t, foundValidSamples, "the query returns some not NaN samples")

				// Run the query with sharding.
				shardedRes, err := shardingware.Wrap(downstream).Do(context.Background(), req)
				require.Nil(t, err)

				// Ensure the two results matches (float precision can slightly differ, there's no guarantee in PromQL engine too
				// if you rerun the same query twice).
				approximatelyEquals(t, expectedRes.(*PrometheusResponse), shardedRes.(*PrometheusResponse))

				// Ensure the query has been sharded/not sharded as expected.
				expectedSharded := 0
				if testData.expectedShardedQueries > 0 {
					expectedSharded = 1
				}

				assert.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(fmt.Sprintf(`
					# HELP cortex_frontend_query_sharding_rewrites_attempted_total Total number of queries the query-frontend attempted to shard.
					# TYPE cortex_frontend_query_sharding_rewrites_attempted_total counter
					cortex_frontend_query_sharding_rewrites_attempted_total 1

					# HELP cortex_frontend_query_sharding_rewrites_succeeded_total Total number of queries the query-frontend successfully rewritten in a shardable way.
					# TYPE cortex_frontend_query_sharding_rewrites_succeeded_total counter
					cortex_frontend_query_sharding_rewrites_succeeded_total %d

					# HELP cortex_frontend_sharded_queries_total Total number of sharded queries.
					# TYPE cortex_frontend_sharded_queries_total counter
					cortex_frontend_sharded_queries_total %d
				`, expectedSharded, testData.expectedShardedQueries*numShards)),
					"cortex_frontend_query_sharding_rewrites_attempted_total",
					"cortex_frontend_query_sharding_rewrites_succeeded_total",
					"cortex_frontend_sharded_queries_total"))
			})
		}
	}
}

func TestQuerySharding_ShouldFallbackToDownstreamHandlerOnMappingFailure(t *testing.T) {
	req := &PrometheusRequest{
		Path:  "/query_range",
		Start: util.TimeToMillis(start),
		End:   util.TimeToMillis(end),
		Step:  step.Milliseconds(),
		Query: "aaa{", // Invalid query.
	}

	shardingware := NewQueryShardingMiddleware(log.NewNopLogger(), newEngine(), 16, nil)

	// Mock the downstream handler, always returning success (regardless the query is valid or not).
	downstream := &mockHandler{}
	downstream.On("Do", mock.Anything, mock.Anything).Return(&PrometheusResponse{Status: StatusSuccess}, nil)

	// Run the query with sharding middleware wrapping the downstream one.
	// We expect the query parsing done by the query sharding middleware to fail
	// but to fallback on the downstream one which always returns success.
	res, err := shardingware.Wrap(downstream).Do(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, StatusSuccess, res.(*PrometheusResponse).GetStatus())
	downstream.AssertCalled(t, "Do", mock.Anything, mock.Anything)
}

func TestQuerySharding_ShouldReturnErrorOnDownstreamHandlerFailure(t *testing.T) {
	req := &PrometheusRequest{
		Path:  "/query_range",
		Start: util.TimeToMillis(start),
		End:   util.TimeToMillis(end),
		Step:  step.Milliseconds(),
		Query: "vector(1)", // A non shardable query.
	}

	shardingware := NewQueryShardingMiddleware(log.NewNopLogger(), newEngine(), 16, nil)

	// Mock the downstream handler to always return error.
	downstreamErr := errors.Errorf("some err")
	downstream := mockHandlerWith(nil, downstreamErr)

	// Run the query with sharding middleware wrapping the downstream one.
	// We expect to get the downstream error.
	_, err := shardingware.Wrap(downstream).Do(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, downstreamErr, err)
}

func BenchmarkQuerySharding(b *testing.B) {
	var shards []int

	// max out at half available cpu cores in order to minimize noisy neighbor issues while benchmarking
	for shard := 1; shard <= runtime.NumCPU()/2; shard = shard * 2 {
		shards = append(shards, shard)
	}

	for _, tc := range []struct {
		labelBuckets     int
		labels           []string
		samplesPerSeries int
		query            string
		desc             string
	}{
		// Ensure you have enough cores to run these tests without blocking.
		// We want to simulate parallel computations and waiting in queue doesn't help

		// no-group
		{
			labelBuckets:     16,
			labels:           []string{"a", "b", "c"},
			samplesPerSeries: 100,
			query:            `sum(rate(http_requests_total[5m]))`,
			desc:             "sum nogroup",
		},
		// sum by
		{
			labelBuckets:     16,
			labels:           []string{"a", "b", "c"},
			samplesPerSeries: 100,
			query:            `sum by(a) (rate(http_requests_total[5m]))`,
			desc:             "sum by",
		},
		// sum without
		{
			labelBuckets:     16,
			labels:           []string{"a", "b", "c"},
			samplesPerSeries: 100,
			query:            `sum without (a) (rate(http_requests_total[5m]))`,
			desc:             "sum without",
		},
	} {
		for _, delayPerSeries := range []time.Duration{
			0,
			time.Millisecond / 10,
		} {
			engine := promql.NewEngine(promql.EngineOpts{
				Logger:     log.NewNopLogger(),
				Reg:        nil,
				MaxSamples: 100000000,
				Timeout:    time.Minute,
			})

			queryable := NewMockShardedQueryable(
				tc.samplesPerSeries,
				tc.labels,
				tc.labelBuckets,
				delayPerSeries,
			)
			downstream := &downstreamHandler{
				engine:    engine,
				queryable: queryable,
			}

			var (
				start int64 = 0
				end         = int64(1000 * tc.samplesPerSeries)
				step        = (end - start) / 1000
			)

			req := &PrometheusRequest{
				Path:    "/query_range",
				Start:   start,
				End:     end,
				Step:    step,
				Timeout: time.Minute,
				Query:   tc.query,
			}

			for _, shardFactor := range shards {
				shardingware := NewQueryShardingMiddleware(
					log.NewNopLogger(),
					engine,
					shardFactor,
					nil,
				).Wrap(downstream)

				b.Run(
					fmt.Sprintf(
						"desc:[%s]---shards:[%d]---series:[%.0f]---delayPerSeries:[%s]---samplesPerSeries:[%d]",
						tc.desc,
						shardFactor,
						math.Pow(float64(tc.labelBuckets), float64(len(tc.labels))),
						delayPerSeries,
						tc.samplesPerSeries,
					),
					func(b *testing.B) {
						for n := 0; n < b.N; n++ {
							_, err := shardingware.Do(
								context.Background(),
								req,
							)
							if err != nil {
								b.Fatal(err.Error())
							}
						}
					},
				)
			}
			fmt.Println()
		}

		fmt.Print("--------------------------------\n\n")
	}
}

type downstreamHandler struct {
	engine    *promql.Engine
	queryable storage.Queryable
}

func (h *downstreamHandler) Do(ctx context.Context, r Request) (Response, error) {
	qry, err := h.engine.NewRangeQuery(
		h.queryable,
		r.GetQuery(),
		util.TimeFromMillis(r.GetStart()),
		util.TimeFromMillis(r.GetEnd()),
		time.Duration(r.GetStep())*time.Millisecond,
	)
	if err != nil {
		return nil, err
	}

	res := qry.Exec(ctx)
	extracted, err := FromResult(res)
	if err != nil {
		return nil, err
	}

	return &PrometheusResponse{
		Status: StatusSuccess,
		Data: PrometheusData{
			ResultType: string(res.Value.Type()),
			Result:     extracted,
		},
	}, nil
}
