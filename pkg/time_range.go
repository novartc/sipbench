package pkg

import (
	"errors"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/novartc/sipbench/pkg/distributor"
)

type TimeRange interface {
	Get() time.Duration
}

type weightedTimeRange interface {
	TimeRange
	getWeight() int
}

type timeSingle struct {
	d      time.Duration
	weight int
}

func (s timeSingle) Get() time.Duration {
	return s.d
}

func (s timeSingle) getWeight() int { return s.weight }

type timeRange struct {
	min, max time.Duration
	weight   int
}

func (r timeRange) Get() time.Duration {
	if r.min >= r.max {
		return r.min
	}
	return time.Duration(rand.Int64N(int64(r.max-r.min))) + r.min
}

func (r timeRange) getWeight() int { return r.weight }

type multiTimeRange struct {
	wrr distributor.Interface[TimeRange]
}

func (r multiTimeRange) Get() time.Duration {
	return r.wrr.Next().Get()
}

// Fixed time (5 seconds): 5
// Time range (5 seconds - 10 seconds): 5-10
// Weighted time (5 seconds with weight 1): 5=1
// Weighted time range (5 seconds - 10 seconds with weight 1): 5-10=1
func newWeightedTimeRange(raw string) (weightedTimeRange, error) {
	var weight int
	i := strings.IndexByte(raw, '=')
	if i != -1 {
		var err error
		weight, err = strconv.Atoi(raw[i+1:])
		if err != nil {
			return nil, err
		}
		raw = raw[:i]
	}

	i = strings.IndexByte(raw, '-')
	if i == -1 {
		d, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		return timeSingle{d: time.Duration(d) * time.Second, weight: weight}, nil
	}

	minD, err := strconv.ParseInt(raw[:i], 10, 64)
	if err != nil {
		return nil, err
	}
	maxD, err := strconv.ParseInt(raw[i+1:], 10, 64)
	if err != nil {
		return nil, err
	}
	return timeRange{min: time.Duration(minD) * time.Second, max: time.Duration(maxD) * time.Second, weight: weight}, nil
}

// NewTimeRange
// Fixed time (5 seconds): 5
// Time range (5 seconds - 10 seconds): 5-10
// Weighted time (5 seconds weight 1, 6 seconds weight 2): 5=1,6=2
// Weighted time range (5-10 seconds weight 1, 6-12 seconds weight 2): 5-10=1,6-12=2
func NewTimeRange(raw string) (TimeRange, error) {
	if raw == "" {
		return nil, errors.New("time range is empty")
	}

	ss := strings.Split(raw, ",")
	var trs []weightedTimeRange

	for _, s := range ss {
		tr, err := newWeightedTimeRange(s)
		if err != nil {
			return nil, err
		}
		trs = append(trs, tr)
	}

	if len(trs) == 1 {
		return trs[0], nil
	}

	rs := make([]distributor.WeightedResource[TimeRange], len(trs))
	for i, tr := range trs {
		weight := tr.getWeight()
		if weight == 0 {
			weight = 1
		}

		rs[i] = distributor.WeightedResource[TimeRange]{
			R:      tr,
			Weight: weight,
		}
	}

	return multiTimeRange{wrr: distributor.NewWeightedRoundRobin[TimeRange](rs)}, nil
}
