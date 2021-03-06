package allocator

import (
	"context"
	"fmt"
	"testing"

	gc "github.com/go-check/check"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"go.gazette.dev/core/etcdtest"
	"go.gazette.dev/core/keyspace"
	"go.gazette.dev/core/metrics"
)

func BenchmarkAll(b *testing.B) {
	b.Run("simulated-deploy", func(b *testing.B) {
		benchmarkSimulatedDeploy(b)
	})
}

type BenchmarkHealthSuite struct{}

// TestBenchmarkHealth runs benchmarks with a small N to ensure they don't bit rot.
func (s *BenchmarkHealthSuite) TestBenchmarkHealth(c *gc.C) {
	var fakeB = testing.B{N: 50}

	benchmarkSimulatedDeploy(&fakeB)
}

var _ = gc.Suite(&BenchmarkHealthSuite{})

func benchmarkSimulatedDeploy(b *testing.B) {
	var client = etcdtest.TestClient()
	defer etcdtest.Cleanup()

	var ctx, _ = context.WithCancel(context.Background())
	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	var state = NewObservedState(ks, MemberKey(ks, "zone-b", "leader"))

	var NItems = b.N

	if NItems < 50 {
		NItems = 50 // Required for the test to work (otherwise NMembers10 == 0).
	}
	var NMembers = NItems / 5
	var NMembersHalf = NMembers / 2
	var NMembers10 = NMembers / 10 // Each stage of the deployment will cycle |NMembers10| Members.

	// fill inserts (if |asInsert|) or modifies keys/values defined by |kvcb| and the range [begin, end).
	var fill = func(begin, end int, asInsert bool, kvcb func(i int) (string, string)) {
		var kv = make([]string, 0, 2*(end-begin))

		for i := begin; i != end; i++ {
			var k, v = kvcb(i)
			kv = append(kv, k)
			kv = append(kv, v)
		}
		if asInsert {
			assert.NoError(b, insert(ctx, client, kv...))
		} else {
			assert.NoError(b, update(ctx, client, kv...))
		}
	}

	// Insert a Member key which will act as the leader, and will not be rolled.
	assert.NoError(b, insert(ctx, client, state.LocalKey, `{"R": 1}`))

	// Announce half of Members...
	fill(0, NMembersHalf, true, func(i int) (string, string) {
		return benchMemberKey(ks, i), `{"R": 45}`
	})
	// And all Items.
	fill(0, NItems, true, func(i int) (string, string) {
		return ItemKey(ks, fmt.Sprintf("i%05d", i)), `{"R": 3}`
	})

	var testState = struct {
		nextBlock  int  // Next block of Members to cycle down & up.
		consistent bool // Whether we've marked Assignments as consistent.
	}{}

	var testHook = func(round int, idle bool) {
		log.WithFields(log.Fields{
			"round":            round,
			"idle":             idle,
			"state.nextBlock":  testState.nextBlock,
			"state.consistent": testState.consistent,
		}).Info("ScheduleCallback")

		if !idle {
			return
		} else if !testState.consistent {
			// Mark any new Assignments as "consistent", which will typically
			// unblock further convergence operations.
			assert.NoError(b, markAllConsistent(ctx, client, ks))
			testState.consistent = true
			return
		}

		var begin, end = NMembers10 * testState.nextBlock, NMembers10 * (testState.nextBlock + 1)
		if begin == NMembersHalf {
			// We've cycled all Members. Gracefully exit by setting our ItemLimit to zero,
			// and waiting for Serve to complete.
			update(ctx, client, state.LocalKey, `{"R": 0}`)
			testState.consistent = false
			return
		}

		// Mark a block of Members as starting up, and shutting down.
		fill(NMembersHalf+begin, NMembersHalf+end, true, func(i int) (string, string) {
			return benchMemberKey(ks, i), `{"R": 45}`
		})
		fill(begin, end, false, func(i int) (string, string) {
			return benchMemberKey(ks, i), `{"R": 0}`
		})

		testState.nextBlock++
		testState.consistent = false
	}

	assert.NoError(b, ks.Load(ctx, client, 0))
	go ks.Watch(ctx, client)

	assert.NoError(b, Allocate(AllocateArgs{
		Context:  ctx,
		Etcd:     client,
		State:    state,
		TestHook: testHook,
	}))

	log.WithFields(log.Fields{
		"adds":    counterVal(metrics.AllocatorAssignmentAddedTotal),
		"removes": counterVal(metrics.AllocatorAssignmentRemovedTotal),
		"packs":   counterVal(metrics.AllocatorAssignmentPackedTotal),
	}).Info("final metrics")
}

func benchMemberKey(ks *keyspace.KeySpace, i int) string {
	var zone string

	switch i % 5 {
	case 0, 2, 4:
		zone = "zone-a"
	case 1, 3:
		zone = "zone-b"
	}
	return MemberKey(ks, zone, fmt.Sprintf("m%05d", i))
}

func counterVal(c prometheus.Counter) float64 {
	var out dto.Metric
	if err := c.Write(&out); err != nil {
		panic(err)
	}
	return *out.Counter.Value
}
