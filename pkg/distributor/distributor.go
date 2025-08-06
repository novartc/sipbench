package distributor

import "sync/atomic"

type Interface[T any] interface {
	Next() T
}

type RoundRobin[T any] struct {
	resources []T
	next      uint32
}

func NewRoundRobin[T any](resources []T) Interface[T] {
	return &RoundRobin[T]{resources: resources}
}

func (r *RoundRobin[T]) Next() T {
	n := atomic.AddUint32(&r.next, 1)
	return r.resources[n%uint32(len(r.resources))]
}

type WeightedResource[T any] struct {
	R      T
	Weight int

	currentWeight int
}

type WeightedRoundRobin[T any] struct {
	resources []WeightedResource[T]
	rr        *RoundRobin[T]
}

func NewWeightedRoundRobin[T any](resources []WeightedResource[T]) Interface[T] {
	wrr := &WeightedRoundRobin[T]{
		resources: resources,
	}

	var totalWeight int
	for _, r := range resources {
		totalWeight += r.Weight
	}
	rs := make([]T, totalWeight)
	for i := 0; i < totalWeight; i++ {
		rs[i] = wrr.calc()
	}

	wrr.rr = &RoundRobin[T]{resources: rs}
	return wrr
}

func (r *WeightedRoundRobin[T]) calc() T {
	var total int
	got := &r.resources[0]

	for i, rr := range r.resources {
		r.resources[i].currentWeight += rr.Weight
		total += rr.Weight

		if rr.currentWeight > got.currentWeight {
			got = &r.resources[i]
		}
	}
	got.currentWeight -= total

	return got.R
}

func (r *WeightedRoundRobin[T]) Next() T { return r.rr.Next() }
