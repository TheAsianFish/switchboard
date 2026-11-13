package budget

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/TheAsianFish/switchboard/internal/cost"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// This file is a seeded fault-schedule simulator for the budget ledger
// (PLAN.md, M11). Several Degrading ledgers, one per replica process,
// share one Redis ledger on an in-process miniredis. A schedule is a list
// of steps (reserve, settle, and seven faults); the simulator runs it on a
// fake clock, brings the system to quiescence, and checks three
// invariants against two sums, what the provider billed and what the
// store holds as spent, plus a log of which faults fired. The reference
// is deliberately not a second ledger: a second ledger would share the
// first one's assumptions.
//
// Schedules are sequential: one step at a time, each ledger call runs to
// completion before the next starts, so interleavings inside one call (a
// flush racing a settle) are out of reach.

// SimOp names one kind of schedule step.
type SimOp string

// The step vocabulary. Reserve and settle are the work; the rest are the
// seven faults: the store unreachable (and back), a replica process killed
// and a fresh one started in its place, a reply lost after the store
// applied the call (on a reserve or on a settle), and three sizes of clock
// step.
const (
	OpReserve     SimOp = "reserve"
	OpSettle      SimOp = "settle"
	OpStoreDown   SimOp = "store-down"
	OpStoreUp     SimOp = "store-up"
	OpKill        SimOp = "kill"
	OpReserveLost SimOp = "reserve-lost"
	OpSettleLost  SimOp = "settle-lost"
	OpClockShort  SimOp = "clock-short"
	OpClockTTL    SimOp = "clock-ttl"
	OpClockRefill SimOp = "clock-refill"
)

// SimFault is one of the seven faults, for counting which ones a schedule
// exercised.
type SimFault string

// The seven faults and what counts as exercising each.
const (
	// FaultOutage: a ledger call was refused because the store was down.
	FaultOutage SimFault = "store unreachable"
	// FaultKill: a replica process was killed and restarted.
	FaultKill SimFault = "replica killed"
	// FaultReserveLost and FaultSettleLost: the store applied a reserve
	// (settle) and its reply was lost on the way back.
	FaultReserveLost SimFault = "reply lost on reserve"
	FaultSettleLost  SimFault = "reply lost on settle"
	// FaultClockShort: the clock stepped a few seconds.
	FaultClockShort SimFault = "clock step, short"
	// FaultClockTTL: the clock stepped past the hold TTL while a hold was
	// open on the store.
	FaultClockTTL SimFault = "clock step past hold TTL"
	// FaultClockRefill: the clock stepped past the grace refill while a
	// live replica's grace allowance was below full.
	FaultClockRefill SimFault = "clock step past grace refill"
)

// SimFaults lists the faults in report order.
var SimFaults = []SimFault{FaultOutage, FaultKill, FaultReserveLost, FaultSettleLost, FaultClockShort, FaultClockTTL, FaultClockRefill}

// SimStep is one step of a schedule.
type SimStep struct {
	Op SimOp `json:"op"`
	// Replica is the replica slot the step acts on (reserve, settle, kill
	// and the lost replies).
	Replica int `json:"replica"`
	// Hold is a reserve's estimate and Cost what the provider bills for
	// the request if it is admitted.
	Hold cost.Nanos `json:"hold,omitempty"`
	Cost cost.Nanos `json:"cost,omitempty"`
	// Advance is a clock step's length.
	Advance time.Duration `json:"advance,omitempty"`
	// N is the step's index in the schedule it was drawn in. A reserve's
	// request id is built from it, so a shrunk schedule keeps its ids.
	N int `json:"n"`
}

// String renders s for a report, e.g. "r1 reserve hold 40 cost 20".
func (s SimStep) String() string {
	switch s.Op {
	case OpReserve, OpReserveLost:
		lost := ""
		if s.Op == OpReserveLost {
			lost = ", reply lost"
		}
		return fmt.Sprintf("r%d reserve q%d hold %d cost %d%s", s.Replica, s.N, s.Hold, s.Cost, lost)
	case OpSettle:
		return fmt.Sprintf("r%d settle oldest", s.Replica)
	case OpSettleLost:
		return fmt.Sprintf("r%d settle oldest, reply lost", s.Replica)
	case OpStoreDown:
		return "store unreachable"
	case OpStoreUp:
		return "store back"
	case OpKill:
		return fmt.Sprintf("r%d killed, new process", s.Replica)
	case OpClockShort, OpClockTTL, OpClockRefill:
		return fmt.Sprintf("clock +%v", s.Advance)
	}
	return string(s.Op)
}

// SimConfig fixes everything about a run except the schedule.
type SimConfig struct {
	Replicas int
	// Limit is the budget (one key, a total budget, so one window).
	Limit cost.Nanos
	// Grace, GraceRefill and RetryInterval configure every Degrading.
	Grace         cost.Nanos
	GraceRefill   time.Duration
	RetryInterval time.Duration
	// HoldTTL is the Redis ledger's reservation TTL.
	HoldTTL time.Duration
	// MaxSteps bounds a schedule's length; lengths are drawn uniformly
	// from 1 to MaxSteps.
	MaxSteps int
	// Weights is the relative chance of each op per step.
	Weights []SimWeight
	// Holds lists the estimates a reserve draws from.
	Holds []cost.Nanos
	// Planted turns on the planted bug: one settle in a hundred (chosen by
	// a hash of the seed and the request id, so shrinking keeps it)
	// releases its hold and records nothing as spent.
	Planted bool
}

// SimWeight is one op's weight in the generator.
type SimWeight struct {
	Op     SimOp `json:"op"`
	Weight int   `json:"weight"`
}

// DefaultSimConfig is the configuration M11's results were run with.
// The limit has to bind inside one schedule or I2 says nothing: a
// schedule makes about seven reserves with a mean hold of about 23, so a
// limit of 100 is used up by most schedules that reserve, and a grace of
// 50 lets about two requests per process through an outage before the
// allowance refuses. The hold TTL and refill are the production defaults.
func DefaultSimConfig() SimConfig {
	return SimConfig{
		Replicas:      3,
		Limit:         100,
		Grace:         50,
		GraceRefill:   DefaultGraceRefill,
		RetryInterval: time.Second,
		HoldTTL:       DefaultReservationTTL,
		MaxSteps:      40,
		Weights: []SimWeight{
			{OpReserve, 30}, {OpSettle, 25},
			{OpStoreDown, 6}, {OpStoreUp, 6}, {OpKill, 5},
			{OpReserveLost, 5}, {OpSettleLost, 5},
			{OpClockShort, 8}, {OpClockTTL, 5}, {OpClockRefill, 5},
		},
		Holds: []cost.Nanos{10, 20, 40},
	}
}

// Generate draws seed's schedule. The same seed and config always give
// the same schedule.
func Generate(seed uint64, cfg SimConfig) []SimStep {
	rng := rand.New(rand.NewPCG(seed, 0x5b0a7d))
	total := 0
	for _, w := range cfg.Weights {
		total += w.Weight
	}
	n := 1 + rng.IntN(cfg.MaxSteps)
	steps := make([]SimStep, 0, n)
	for i := range n {
		pick := rng.IntN(total)
		var op SimOp
		for _, w := range cfg.Weights {
			if pick < w.Weight {
				op = w.Op
				break
			}
			pick -= w.Weight
		}
		s := SimStep{Op: op, N: i}
		switch op {
		case OpReserve, OpReserveLost:
			s.Replica = rng.IntN(cfg.Replicas)
			s.Hold = cfg.Holds[rng.IntN(len(cfg.Holds))]
			// Most requests cost at most their hold; one in ten costs half
			// as much again (an admitted under-estimate, which I2 allows
			// for), and one in twenty nothing (a failed request).
			switch u := rng.IntN(20); {
			case u < 2:
				s.Cost = s.Hold * 3 / 2
			case u < 3:
				s.Cost = 0
			default:
				s.Cost = s.Hold * cost.Nanos(1+rng.IntN(4)) / 4
			}
		case OpSettle, OpSettleLost, OpKill:
			s.Replica = rng.IntN(cfg.Replicas)
		case OpClockShort:
			s.Advance = time.Duration(1000+rng.IntN(4000)) * time.Millisecond
		case OpClockTTL:
			s.Advance = cfg.HoldTTL + time.Duration(1+rng.IntN(60))*time.Second
		case OpClockRefill:
			s.Advance = cfg.GraceRefill + time.Duration(1+rng.IntN(60))*time.Second
		}
		steps = append(steps, s)
	}
	return steps
}

// SimOutcome is what one run of a schedule found at quiescence.
type SimOutcome struct {
	// Billed is what the provider billed: the cost of every admitted
	// request. Spent and Reserved are the store's balance at quiescence.
	Billed, Spent, Reserved cost.Nanos
	// Under is the sum of what admitted requests cost above their holds.
	Under cost.Nanos
	// Processes counts replica processes started; Elapsed is the clock's
	// advance over the schedule (quiescence excluded).
	Processes int
	Elapsed   time.Duration
	// Bound is I2's allowance over the limit, in nanodollars, rounded
	// down: Processes × Grace × (1 + Elapsed / GraceRefill) + Under.
	Bound cost.Nanos
	// I1 to I4 report whether each invariant held.
	I1, I2, I3, I4 bool
	// Exercised lists the faults this run exercised, in SimFaults order.
	Exercised []SimFault
	// LostReply is true if any reply was lost after the store applied
	// the call. KilledPending is true if a killed process held pending
	// spend. Expired lists holds that expired on the store unsettled.
	LostReply     bool
	KilledPending bool
	Expired       []string
	// Planted counts settles the planted bug emptied.
	Planted int
	// LostPending, LostRedisHold and LostGraceHold are the fault log of
	// every kill: the pending spend the killed process held that the
	// store had not already applied and that no hold on the store stands
	// for, what its requests with a hold on the store cost above that
	// hold, and the cost of its requests in flight on its grace
	// allowance. A Redis hold whose request died is never settled, so it
	// expires and the store charges the held amount: the request is lost
	// only where it cost more than its hold.
	LostPending, LostRedisHold, LostGraceHold cost.Nanos
	// OverExpired is the other side of a dead request's expired hold:
	// what the held amount charged exceeds the request's cost.
	OverExpired cost.Nanos
	// Orphaned is what the store charged for holds left by a lost reply
	// on a reserve: the replica admitted the request on grace and never
	// settled the hold, which expired.
	Orphaned cost.Nanos
	// ExpiredBound is held − cost summed over every hold that expired,
	// where the hold exceeds the cost, for requests admitted on the
	// store: the most the expiry charges can over-count (I4).
	ExpiredBound cost.Nanos
	// Doubled is what the store charged twice: the spend a call applied
	// before its reply was lost (read from the store around the call),
	// which the replica kept as pending and later flushed again. Pending
	// of that kind dying with its process is not counted in LostPending.
	Doubled cost.Nanos
	// PlantedLost is what the planted bug kept out of spent: the cost of
	// each emptied settle the store applied and the replica heard back
	// from. One whose reply was lost is kept as pending and charged
	// again, so it loses nothing.
	PlantedLost cost.Nanos
	// Trace is one line per step and per quiescence action, filled only
	// when asked for.
	Trace []string
}

// Holds reports whether invariant inv ("I1", "I2" or "I3") held.
func (o SimOutcome) Holds(inv string) bool {
	switch inv {
	case "I1":
		return o.I1
	case "I2":
		return o.I2
	case "I3":
		return o.I3
	case "I4":
		return o.I4
	}
	return true
}

// Classify applies PLAN.md's rule to the run of a minimal schedule:
// "killed pending" if it kills a process that holds pending spend,
// "expired hold" if a hold expires unsettled, otherwise "other".
func (o SimOutcome) Classify() string {
	switch {
	case o.KilledPending:
		return ClassKilledPending
	case len(o.Expired) > 0:
		return ClassExpiredHold
	}
	return ClassOther
}

// Shortfall is billed − spent: the spend the store never recorded.
func (o SimOutcome) Shortfall() cost.Nanos { return o.Billed - o.Spent }

// Lost is the spend that never reached the store before charges above the
// cost made up some of it: billed − spent + Doubled + Orphaned +
// OverExpired. Without adding those back, a lost reply's second charge or
// a dead request's over-estimated hold would hide a loss of the same size
// somewhere else.
func (o SimOutcome) Lost() cost.Nanos {
	return o.Shortfall() + o.Doubled + o.Orphaned + o.OverExpired
}

// Unexplained is the loss the two listed ways of losing spend do not
// account for: what remains after the pending spend of every killed
// process and what its requests with a Redis hold cost above the hold.
func (o SimOutcome) Unexplained() cost.Nanos {
	return o.Lost() - o.LostPending - o.LostRedisHold
}

// Unaccounted is the loss no kill accounts for: what remains after
// everything every killed process held, its grace holds in flight
// included. Negative if the store holds more than billed, the recorded
// losses and the double charges explain.
func (o SimOutcome) Unaccounted() cost.Nanos {
	return o.Unexplained() - o.LostGraceHold
}

// Balanced reports whether the run's money is fully accounted for:
// spent = billed − every kill's losses − the planted bug's + Doubled +
// Orphaned + OverExpired.
// A run that is not balanced lost or invented spend in a way the fault
// log does not record.
func (o SimOutcome) Balanced() bool { return o.Unaccounted() == o.PlantedLost }

// Classes reports which ways of losing spend the run needs, read from the
// fault log rather than from a shrunk schedule. A way is needed when the
// loss (Lost) is more than every other recorded loss could cover: "killed
// pending" when it exceeds the killed processes' holds in flight (Redis
// and grace), "expired hold" when it exceeds their pending spend and
// grace holds, and "other" when it exceeds both listed ways together.
// Since expired holds are charged (M12), "expired hold" is only what a
// dead request cost above its hold. Nil if no spend was lost.
func (o SimOutcome) Classes() []string {
	if o.I1 {
		return nil
	}
	d := o.Lost()
	var c []string
	if o.LostPending > 0 && d > o.LostRedisHold+o.LostGraceHold {
		c = append(c, ClassKilledPending)
	}
	if o.LostRedisHold > 0 && d > o.LostPending+o.LostGraceHold {
		c = append(c, ClassExpiredHold)
	}
	if o.Unexplained() > 0 {
		c = append(c, ClassOther)
	}
	if o.Unaccounted() > 0 {
		c = append(c, ClassUnaccounted)
	}
	return c
}

// The classes of PLAN.md's rule.
const (
	ClassKilledPending = "killed pending"
	ClassExpiredHold   = "expired hold"
	ClassOther         = "other"
	// ClassUnaccounted is a diagnostic, not one of the rule's classes:
	// spend lost beyond everything the killed processes held.
	ClassUnaccounted = "unaccounted"
)

// Invariants lists the invariants in report order.
var Invariants = []string{"I1", "I2", "I3", "I4"}

// Simulator runs schedules on one miniredis. It is not safe for
// concurrent use; run one per goroutine.
type Simulator struct {
	cfg SimConfig
	m   *miniredis.Miniredis
	rdb *redis.Client
	ctx context.Context
	// runs counts schedules executed, shrinking included.
	runs int
}

// simT0 is where every run's clock starts.
var simT0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

const (
	simKey    = "k"
	simPrefix = "sim:"
)

// NewSimulator starts an in-process miniredis for cfg's runs.
func NewSimulator(cfg SimConfig) (*Simulator, error) {
	if cfg.Replicas < 1 || cfg.MaxSteps < 1 || len(cfg.Holds) == 0 || len(cfg.Weights) == 0 {
		return nil, errors.New("budget sim: config needs replicas, steps, holds and weights")
	}
	m := miniredis.NewMiniRedis()
	rdb := redis.NewClient(&redis.Options{
		Dialer:          func(context.Context, string, string) (net.Conn, error) { return m.Dial() },
		PoolSize:        1,
		DisableIdentity: true,
		MaxRetries:      -1,
	})
	// Degrading logs every transition at warn; a run has hundreds.
	ctx := telemetry.WithRequest(context.Background(), slog.New(slog.DiscardHandler), "sim")
	return &Simulator{cfg: cfg, m: m, rdb: rdb, ctx: ctx}, nil
}

// Close stops the simulator's miniredis.
func (s *Simulator) Close() {
	_ = s.rdb.Close()
	s.m.Close()
}

// Runs reports how many schedules the simulator has executed.
func (s *Simulator) Runs() int { return s.runs }

// Config returns the simulator's configuration.
func (s *Simulator) Config() SimConfig { return s.cfg }

// Run executes steps for seed (the seed only picks the planted bug's
// victims) and checks the invariants at quiescence. With trace set the
// outcome carries a line per step.
func (s *Simulator) Run(seed uint64, steps []SimStep, trace bool) (SimOutcome, error) {
	s.runs++
	s.m.FlushAll()
	r := &simRun{sim: s, seed: seed, now: simT0, trace: trace, expired: map[string]bool{}, exercised: map[SimFault]bool{},
		redis: map[string]simReq{}, orphans: map[string]bool{}, charged: map[string]cost.Nanos{}}
	s.m.SetTime(simT0)
	keys := (&Redis{prefix: simPrefix}).keys(simKey, s.budget().Window(simT0))
	r.balKey, r.expKey, r.amtKey = keys[0], keys[1], keys[2]
	for range s.cfg.Replicas {
		r.replicas = append(r.replicas, r.start())
	}
	for _, st := range steps {
		if err := r.step(st); err != nil {
			return SimOutcome{}, fmt.Errorf("budget sim: step %v: %w", st, err)
		}
	}
	elapsed := r.now.Sub(simT0)
	if err := r.quiesce(); err != nil {
		return SimOutcome{}, fmt.Errorf("budget sim: quiesce: %w", err)
	}
	spent, err1 := strconv.ParseInt(orZero(s.m.HGet(r.balKey, "spent")), 10, 64)
	reserved, err2 := strconv.ParseInt(orZero(s.m.HGet(r.balKey, "reserved")), 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return SimOutcome{}, fmt.Errorf("budget sim: read balance: %w", err)
	}
	if reserved != 0 {
		return SimOutcome{}, fmt.Errorf("budget sim: %d still reserved at quiescence", reserved)
	}
	o := SimOutcome{
		Billed:        r.billed,
		Spent:         cost.Nanos(spent),
		Reserved:      cost.Nanos(reserved),
		Under:         r.under,
		Processes:     r.procs,
		Elapsed:       elapsed,
		LostReply:     r.lostReply,
		KilledPending: r.killedPending,
		Planted:       r.planted,
		LostPending:   r.lostPending,
		LostRedisHold: r.lostRedis,
		LostGraceHold: r.lostGrace,
		Doubled:       r.doubled,
		OverExpired:   r.overExpired,
		Orphaned:      r.orphaned,
		ExpiredBound:  r.expiredBound,
		PlantedLost:   r.plantedLost,
		Trace:         r.lines,
	}
	for id := range r.expired {
		o.Expired = append(o.Expired, id)
	}
	slices.Sort(o.Expired)
	for _, f := range SimFaults {
		if r.exercised[f] {
			o.Exercised = append(o.Exercised, f)
		}
	}
	o.Bound, o.I2 = s.bound(o)
	o.I1 = o.Spent >= o.Billed
	// Spend above billed is only allowed where a lost reply's applied call
	// was charged again or its orphaned hold expired, and where an
	// expired hold was charged above its request's cost, and only by that
	// much.
	o.I3 = o.Spent <= o.Billed+o.Doubled+o.Orphaned+o.ExpiredBound
	// Without a lost reply, the expiry charges are the only over-count.
	o.I4 = o.LostReply || o.Spent-o.Billed <= o.ExpiredBound
	return o, nil
}

// bound computes I2's allowance and whether billed stays within the limit
// plus it, exactly: billed × R ≤ (limit + P × G + U) × R + P × G × T.
func (s *Simulator) bound(o SimOutcome) (cost.Nanos, bool) {
	c := s.cfg
	pg := new(big.Int).Mul(big.NewInt(int64(o.Processes)), big.NewInt(int64(c.Grace)))
	refill := big.NewInt(int64(c.GraceRefill))
	// allowance × R = P × G × (R + T) + U × R
	allowance := new(big.Int).Mul(pg, new(big.Int).Add(refill, big.NewInt(int64(o.Elapsed))))
	allowance.Add(allowance, new(big.Int).Mul(big.NewInt(int64(o.Under)), refill))
	lhs := new(big.Int).Mul(big.NewInt(int64(o.Billed-c.Limit)), refill)
	held := lhs.Cmp(allowance) <= 0
	return cost.Nanos(new(big.Int).Quo(allowance, refill).Int64()), held
}

func (s *Simulator) budget() Budget { return Budget{Limit: s.cfg.Limit, Period: PeriodTotal} }

func orZero(v string) string {
	if v == "" {
		return "0"
	}
	return v
}

// simRun is the state of one run.
type simRun struct {
	sim      *Simulator
	seed     uint64
	now      time.Time
	down     bool
	replicas []*simReplica
	procs    int

	billed, under cost.Nanos

	balKey, expKey, amtKey string
	exercised              map[SimFault]bool
	expired                map[string]bool
	lostReply              bool
	killedPending          bool
	planted                int

	lostPending, lostRedis, lostGrace cost.Nanos
	doubled, plantedLost              cost.Nanos
	overExpired, orphaned             cost.Nanos
	expiredBound                      cost.Nanos

	// redis maps the id of every request admitted on the store to it;
	// orphans holds the ids of holds a lost reply left on the store; and
	// charged maps every hold that expired to the amount the store
	// charged for it.
	redis   map[string]simReq
	orphans map[string]bool
	charged map[string]cost.Nanos

	// victim is set while the planted bug has emptied a settle (of
	// victimCost) and the store has not yet answered.
	victim     bool
	victimCost cost.Nanos

	trace bool
	lines []string
}

// simReplica is one replica slot: the process running in it and that
// process's requests in flight, oldest first.
type simReplica struct {
	d        *Degrading
	conn     *simConn
	gen      int
	inflight []simReq
	// dup is the part of the process's plain pending spend the store
	// already applied (a lost reply's call): flushing it charges it
	// twice, and losing it with the process loses nothing. dupHold is the
	// same for pending spend kept under a Redis hold's id, per id.
	dup     cost.Nanos
	dupHold map[string]dupHold
}

// dupHold is pending spend under a hold's id whose settle the store
// applied before the reply was lost: amount is the pending cost, store
// what the store holds for the request (the expiry charge, if any, plus
// what the lost call added).
type dupHold struct{ amount, store cost.Nanos }

type simReq struct {
	res  Reservation
	cost cost.Nanos
}

func (r *simRun) Now() time.Time { return r.now }

func (r *simRun) logf(format string, args ...any) {
	if r.trace {
		r.lines = append(r.lines, fmt.Sprintf(format, args...))
	}
}

func (r *simRun) advance(d time.Duration) {
	r.now = r.now.Add(d)
	r.sim.m.SetTime(r.now)
}

// start starts a replica process: a Degrading over its own connection to
// the shared store.
func (r *simRun) start() *simReplica {
	r.procs++
	c := r.sim.cfg
	conn := &simConn{run: r, inner: r.sim.rdb}
	led := NewRedis(conn, simPrefix, c.HoldTTL, r.Now)
	if c.Planted {
		led.dropSettle = r.dropSettle
	}
	d := NewDegrading(led, DegradeOptions{
		Grace:         c.Grace,
		GraceRefill:   c.GraceRefill,
		RetryInterval: c.RetryInterval,
		// Every primary call is in-process; a real-time timeout would
		// only add flakiness under -race.
		Timeout: 10 * time.Second,
		Now:     r.Now,
	})
	return &simReplica{d: d, conn: conn, gen: r.procs, dupHold: map[string]dupHold{}}
}

// dropSettle is the planted bug's choice: one request id in a hundred,
// by a hash of the seed and the id.
func (r *simRun) dropSettle(id string, actual cost.Nanos) bool {
	if !r.picks(id) {
		return false
	}
	// It counts as fired once the store applies it (simConn.do): a settle
	// refused by a down store changes nothing.
	r.victim, r.victimCost = true, actual
	return true
}

// picks reports whether the planted bug empties id's settles.
func (r *simRun) picks(id string) bool {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], r.seed)
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(id))
	return h.Sum64()%100 == 0
}

// track runs after every ledger call on rep: a lost reply's applied spend
// on a plain charge becomes duplicate pending, and duplicate pending that
// a flush charged is a double charge. A lost settle of a Redis hold is
// recorded by the settle step itself (dupHold).
func (r *simRun) track(rep *simReplica) {
	plain, held := rep.d.pendingSplit()
	if l := rep.conn.lostSpent; l > 0 && rep.conn.lostPlain {
		rep.dup = min(rep.dup+l, plain)
	}
	rep.conn.lostSpent, rep.conn.lostPlain = 0, false
	if plain == 0 && rep.dup > 0 {
		r.doubled += rep.dup
		rep.dup = 0
	}
	for id, dh := range rep.dupHold {
		if _, ok := held[id]; ok {
			continue
		}
		// Flushed: the store had no hold or tombstone left for it, so the
		// flush charged the cost again, unless the planted bug emptied
		// that settle too (it picks by id, so it empties both).
		delete(rep.dupHold, id)
		if !r.sim.cfg.Planted || !r.picks(id) {
			r.doubled += dh.amount
		}
	}
}

func (r *simRun) step(st SimStep) error {
	ctx := r.sim.ctx
	b := r.sim.budget()
	switch st.Op {
	case OpReserve, OpReserveLost:
		rep := r.replicas[st.Replica]
		id := "q" + strconv.Itoa(st.N)
		rep.conn.lose = st.Op == OpReserveLost
		res, err := rep.d.Reserve(ctx, simKey, b, id, st.Hold)
		r.track(rep)
		lost := rep.conn.disarm()
		if lost {
			r.exercised[FaultReserveLost] = true
			if r.sim.m.HGet(r.amtKey, id) != "" {
				r.orphans[id] = true
			}
		}
		switch {
		case errors.Is(err, ErrExhausted):
			r.logf("%v: refused%s", st, lostNote(lost))
			return nil
		case err != nil:
			return err
		}
		r.billed += st.Cost
		r.under += max(0, st.Cost-st.Hold)
		q := simReq{res: res, cost: st.Cost}
		rep.inflight = append(rep.inflight, q)
		if !res.grace {
			r.redis[id] = q
		}
		where := "on Redis"
		if res.grace {
			where = "on grace"
		}
		r.logf("%v: admitted %s, provider bills %d%s", st, where, st.Cost, lostNote(lost))
	case OpSettle, OpSettleLost:
		rep := r.replicas[st.Replica]
		if len(rep.inflight) == 0 {
			r.logf("%v: nothing in flight", st)
			return nil
		}
		q := rep.inflight[0]
		rep.inflight = rep.inflight[1:]
		rep.conn.lose = st.Op == OpSettleLost
		where, err := r.settle(rep, q)
		lost := rep.conn.disarm()
		if lost {
			r.exercised[FaultSettleLost] = true
		}
		if err != nil {
			return err
		}
		r.logf("%v: %s %d %s%s", st, q.res.ID, q.cost, where, lostNote(lost))
	case OpStoreDown:
		r.down = true
		r.logf("%v", st)
	case OpStoreUp:
		r.down = false
		r.logf("%v", st)
	case OpKill:
		rep := r.replicas[st.Replica]
		r.exercised[FaultKill] = true
		plain, held := rep.d.pendingSplit()
		if plain > 0 {
			r.killedPending = true
		}
		// The part the store already applied dies without loss.
		lost := plain - rep.dup
		for id, amount := range held {
			if amount > 0 {
				r.killedPending = true
			}
			if dh, ok := rep.dupHold[id]; ok {
				// The store applied this settle and lost the reply: it
				// already holds what it will ever hold for the request.
				lost += dh.amount - dh.store
				continue
			}
			// The hold is on the store, live or as a tombstone, and will
			// have been charged at its held amount.
			r.expiredCharge(r.redis[id].res.Amount, amount)
		}
		r.lostPending += lost
		var inflight []string
		for _, q := range rep.inflight {
			kind := "Redis hold"
			if q.res.grace {
				kind = "grace hold"
				r.lostGrace += q.cost
			} else {
				r.expiredCharge(q.res.Amount, q.cost)
			}
			inflight = append(inflight, fmt.Sprintf("%s (%s, cost %d)", q.res.ID, kind, q.cost))
		}
		applied := ""
		if rep.dup > 0 {
			applied = fmt.Sprintf(" (%d more already on the store)", rep.dup)
		}
		r.replicas[st.Replica] = r.start()
		r.logf("%v: pending %d lost%s; in flight: %s", st, lost, applied, orNone(inflight))
	case OpClockShort:
		r.exercised[FaultClockShort] = true
		r.advance(st.Advance)
		r.logf("%v", st)
	case OpClockTTL:
		if r.holdsOpen() {
			r.exercised[FaultClockTTL] = true
		}
		r.advance(st.Advance)
		r.logf("%v (past hold TTL)", st)
	case OpClockRefill:
		for _, rep := range r.replicas {
			if rep.d.belowFull() {
				r.exercised[FaultClockRefill] = true
			}
		}
		r.advance(st.Advance)
		r.logf("%v (past grace refill)", st)
	default:
		return fmt.Errorf("unknown op %q", st.Op)
	}
	return nil
}

// expiredCharge records a dead request's hold, charged by the store at
// its held amount: the part above the cost is over-counted, the part
// below is lost.
func (r *simRun) expiredCharge(hold, c cost.Nanos) {
	r.overExpired += max(0, hold-c)
	r.lostRedis += max(0, c-hold)
}

// settle settles q on rep and says where its cost went.
func (r *simRun) settle(rep *simReplica, q simReq) (string, error) {
	before := rep.d.pendingTotal()
	planted := r.planted
	rep.conn.lostPlain = q.res.grace
	err := rep.d.Settle(r.sim.ctx, simKey, r.sim.budget(), q.res, q.cost)
	r.victim = false
	if rep.conn.lost && !q.res.grace {
		// The store applied the settle of a Redis hold and the reply was
		// lost: the cost waits in pending under the hold's id.
		if _, held := rep.d.pendingSplit(); held != nil {
			if _, ok := held[q.res.ID]; ok {
				rep.dupHold[q.res.ID] = dupHold{amount: q.cost, store: r.charged[q.res.ID] + rep.conn.lostSpent}
			}
		}
	}
	r.track(rep)
	if err != nil {
		return "", err
	}
	where := "settled on Redis"
	if rep.d.pendingTotal() > before {
		where = "kept as pending"
	}
	if r.planted > planted {
		where = "planted bug: the store charged nothing; " + where
	}
	return where, nil
}

// quiesce brings the run to the state the invariants are checked in: the
// store reachable, every live request settled, every live replica out of
// degraded mode with nothing pending, and every hold settled or expired.
func (r *simRun) quiesce() error {
	ctx := r.sim.ctx
	b := r.sim.budget()
	r.down = false
	r.logf("quiesce: store reachable")
	for i, rep := range r.replicas {
		for _, q := range rep.inflight {
			where, err := r.settle(rep, q)
			if err != nil {
				return err
			}
			r.logf("quiesce: r%d settle %s %d %s", i, q.res.ID, q.cost, where)
		}
		rep.inflight = nil
	}
	for round := 0; ; round++ {
		r.advance(r.sim.cfg.RetryInterval)
		done := true
		for _, rep := range r.replicas {
			_, err := rep.d.Balance(ctx, simKey, b)
			r.track(rep)
			if err != nil && !errors.Is(err, ErrUnavailable) {
				return err
			}
			if rep.d.Degraded() || rep.d.pendingTotal() != 0 {
				done = false
			}
		}
		if done {
			break
		}
		if round == 3 {
			return errors.New("replicas still degraded or pending after three probes")
		}
	}
	r.logf("quiesce: every replica flushed")
	r.advance(r.sim.cfg.HoldTTL + time.Second)
	for _, rep := range r.replicas {
		_, err := rep.d.Balance(ctx, simKey, b)
		r.track(rep)
		if err != nil {
			return err
		}
	}
	r.logf("quiesce: clock past hold TTL")
	return nil
}

// holdsOpen reports whether the store has any hold.
func (r *simRun) holdsOpen() bool {
	m, err := r.sim.m.SortedSet(r.expKey)
	return err == nil && len(m) > 0
}

// noteExpired records the holds the next script will purge as expired
// and returns what their expiry will charge: every script starts by
// charging the holds whose expiry is at or before Redis TIME, so a hold
// seen here has expired unsettled.
func (r *simRun) noteExpired() cost.Nanos {
	m, err := r.sim.m.SortedSet(r.expKey)
	if err != nil {
		return 0
	}
	now := float64(r.now.UnixMilli())
	var charge cost.Nanos
	for id, at := range m {
		if at > now {
			continue
		}
		amount, _ := strconv.ParseInt(orZero(r.sim.m.HGet(r.amtKey, id)), 10, 64)
		charge += cost.Nanos(amount)
		if r.expired[id] {
			continue
		}
		r.expired[id] = true
		r.charged[id] = cost.Nanos(amount)
		switch q, ok := r.redis[id]; {
		case ok:
			r.expiredBound += max(0, cost.Nanos(amount)-q.cost)
		case r.orphans[id]:
			r.orphaned += cost.Nanos(amount)
		}
	}
	return charge
}

func lostNote(lost bool) string {
	if lost {
		return " (reply lost after the store applied it)"
	}
	return ""
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// pendingTotal is the spend d holds in memory for the primary.
func (d *Degrading) pendingTotal() cost.Nanos {
	d.mu.Lock()
	defer d.mu.Unlock()
	var t cost.Nanos
	for _, p := range d.pending {
		t += p.amount
	}
	return t
}

// pendingSplit is d's pending spend in two parts: the plain sum, and the
// spend kept under a Redis hold's id, by id (nil if none).
func (d *Degrading) pendingSplit() (cost.Nanos, map[string]cost.Nanos) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var plain cost.Nanos
	var held map[string]cost.Nanos
	for k, p := range d.pending {
		if k.id == "" {
			plain += p.amount
			continue
		}
		if held == nil {
			held = map[string]cost.Nanos{}
		}
		held[k.id] += p.amount
	}
	return plain, held
}

// belowFull reports whether any key's grace allowance is below full.
func (d *Degrading) belowFull() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.buckets) > 0
}

// errSimUnreachable is what a call to a down store gets.
var errSimUnreachable = errors.New("dial tcp: connect: connection refused (simulated outage)")

// errSimLost is what a call whose reply was lost gets.
var errSimLost = errors.New("read tcp: i/o timeout (simulated: the store applied the call, the reply was lost)")

// simConn is one replica process's connection to the shared store. It
// refuses every call while the store is down and, when lose is set, drops
// the reply of the next call the store applies.
type simConn struct {
	run   *simRun
	inner redis.Scripter
	// lose is armed by a lost-reply step; lost records that it fired and
	// lostSpent what the call it fired on added to spent, apart from the
	// expiry charges it made. lostPlain marks a call whose cost the
	// replica keeps as plain pending if the reply is lost.
	lose, lost, lostPlain bool
	lostSpent             cost.Nanos
}

var _ redis.Scripter = (*simConn)(nil)

// disarm clears a lost-reply step's arming and reports whether it fired.
func (c *simConn) disarm() bool {
	lost := c.lost
	c.lose, c.lost = false, false
	return lost
}

func (c *simConn) do(ctx context.Context, call func() *redis.Cmd) *redis.Cmd {
	r := c.run
	if r.down {
		r.exercised[FaultOutage] = true
		r.victim = false
		cmd := redis.NewCmd(ctx)
		cmd.SetErr(errSimUnreachable)
		return cmd
	}
	expiring := r.noteExpired()
	before := r.spent()
	cmd := call()
	if errors.Is(cmd.Err(), redis.ErrNoScript) || redis.HasErrorPrefix(cmd.Err(), "NOSCRIPT") {
		// Not run; go-redis loads the script and calls again.
		return cmd
	}
	lose := c.lose
	if r.victim {
		r.victim = false
		r.planted++
		if !lose {
			r.plantedLost += r.victimCost
		}
	}
	if lose {
		c.lose, c.lost = false, true
		c.lostSpent = r.spent() - before - expiring
		r.lostReply = true
		lost := redis.NewCmd(ctx)
		lost.SetErr(errSimLost)
		return lost
	}
	return cmd
}

// spent reads the store's spent for the run's key directly.
func (r *simRun) spent() cost.Nanos {
	v, err := strconv.ParseInt(orZero(r.sim.m.HGet(r.balKey, "spent")), 10, 64)
	if err != nil {
		return 0
	}
	return cost.Nanos(v)
}

func (c *simConn) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	return c.do(ctx, func() *redis.Cmd { return c.inner.Eval(ctx, script, keys, args...) })
}

func (c *simConn) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *redis.Cmd {
	return c.do(ctx, func() *redis.Cmd { return c.inner.EvalSha(ctx, sha1, keys, args...) })
}

func (c *simConn) EvalRO(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	return c.do(ctx, func() *redis.Cmd { return c.inner.EvalRO(ctx, script, keys, args...) })
}

func (c *simConn) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...any) *redis.Cmd {
	return c.do(ctx, func() *redis.Cmd { return c.inner.EvalShaRO(ctx, sha1, keys, args...) })
}

func (c *simConn) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	return c.inner.ScriptExists(ctx, hashes...)
}

func (c *simConn) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	return c.inner.ScriptLoad(ctx, script)
}

// SimViolation is one invariant a seed broke, shrunk and classified by
// PLAN.md's rule.
type SimViolation struct {
	Invariant string
	// Class is PLAN.md's rule applied to the minimal schedule's run.
	Class string
	// Minimal is the schedule after greedy shrinking: removing any one
	// step makes the invariant hold.
	Minimal []SimStep
	// Outcome is the minimal schedule's run, traced.
	Outcome SimOutcome
}

// SimClassShrink is a lost-spend seed shrunk while keeping one class's
// loss: the shrinker may drop a step only if spend is still lost and the
// class's own loss is still there, so it cannot slip from one way of
// losing spend to another.
type SimClassShrink struct {
	Class   string
	Minimal []SimStep
	Outcome SimOutcome
}

// SimSeedResult is one seed's schedule, its run and what it broke.
type SimSeedResult struct {
	Seed    uint64
	Steps   int
	Outcome SimOutcome
	// Violations has one entry per invariant broken, under PLAN.md's rule.
	Violations []SimViolation
	// Classes has one entry per class the seed's lost spend falls in
	// (SimOutcome.Classes), each shrunk without slipping.
	Classes []SimClassShrink
}

// Check draws seed's schedule, runs it, and shrinks and classifies every
// invariant it breaks; with classes set it also shrinks the lost spend
// once per class it falls in.
func (s *Simulator) Check(seed uint64, classes bool) (SimSeedResult, error) {
	steps := Generate(seed, s.cfg)
	o, err := s.Run(seed, steps, false)
	if err != nil {
		return SimSeedResult{}, fmt.Errorf("seed %d: %w", seed, err)
	}
	res := SimSeedResult{Seed: seed, Steps: len(steps), Outcome: o}
	for _, inv := range Invariants {
		if o.Holds(inv) {
			continue
		}
		minimal, mo, err := s.Shrink(seed, steps, func(o SimOutcome) bool { return !o.Holds(inv) })
		if err != nil {
			return SimSeedResult{}, fmt.Errorf("seed %d: shrink %s: %w", seed, inv, err)
		}
		res.Violations = append(res.Violations, SimViolation{Invariant: inv, Class: mo.Classify(), Minimal: minimal, Outcome: mo})
	}
	if !classes {
		return res, nil
	}
	for _, c := range o.Classes() {
		minimal, mo, err := s.Shrink(seed, steps, func(o SimOutcome) bool { return slices.Contains(o.Classes(), c) })
		if err != nil {
			return SimSeedResult{}, fmt.Errorf("seed %d: shrink %s: %w", seed, c, err)
		}
		res.Classes = append(res.Classes, SimClassShrink{Class: c, Minimal: minimal, Outcome: mo})
	}
	return res, nil
}

// Shrink removes steps from a schedule whose run satisfies fails for as
// long as the run still does: first runs of steps, halving their length
// down to two, then single steps until no single removal keeps it. The
// result is 1-minimal: every remaining step is needed. It returns the
// minimal schedule and its traced run.
func (s *Simulator) Shrink(seed uint64, steps []SimStep, fails func(SimOutcome) bool) ([]SimStep, SimOutcome, error) {
	cur := slices.Clone(steps)
	try := func(i, n int) (bool, error) {
		cand := slices.Concat(cur[:i], cur[i+n:])
		o, err := s.Run(seed, cand, false)
		if err != nil {
			return false, err
		}
		if fails(o) {
			cur = cand
			return true, nil
		}
		return false, nil
	}
	for n := len(cur) / 2; n >= 2; n /= 2 {
		for i := 0; i+n <= len(cur); {
			ok, err := try(i, n)
			if err != nil {
				return nil, SimOutcome{}, err
			}
			if !ok {
				i += n
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for i := len(cur) - 1; i >= 0; i-- {
			ok, err := try(i, 1)
			if err != nil {
				return nil, SimOutcome{}, err
			}
			changed = changed || ok
		}
	}
	o, err := s.Run(seed, cur, true)
	if err != nil {
		return nil, SimOutcome{}, err
	}
	if !fails(o) {
		return nil, SimOutcome{}, errors.New("minimal schedule no longer fails")
	}
	return cur, o, nil
}
