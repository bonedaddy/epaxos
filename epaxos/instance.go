package epaxos

import (
	"sort"

	"github.com/google/btree"

	pb "github.com/nvanbenschoten/epaxos/epaxos/epaxospb"
)

type instanceState int

//go:generate stringer -type=instanceState
const (
	none instanceState = iota
	preAccepted
	accepted
	committed
	executed
)

type instance struct {
	p      *epaxos
	r      pb.ReplicaID
	i      pb.InstanceNum
	cmd    pb.Command
	seq    pb.SeqNum
	deps   map[pb.Dependency]struct{}
	ballot pb.Ballot
	state  instanceState

	// command-leader state
	preAcceptReplies int
	differentReplies bool
	slowPathTimer    tickingTimer
	acceptReplies    int
}

// TODO restructure state machine

const slowPathTimout = 2

func (p *epaxos) newInstance(r pb.ReplicaID, i pb.InstanceNum) *instance {
	inst := &instance{p: p, r: r, i: i}
	inst.slowPathTimer = makeTickingTimer(slowPathTimout, func() {
		inst.transitionToAccept()
	})
	return inst
}

//
// BTree Functions
//

// Len implements the btree.Item interface.
func (inst *instance) Less(than btree.Item) bool {
	return inst.i < than.(*instance).i
}

// instanceKey creates a key to index into the commands btree.
func instanceKey(i pb.InstanceNum) btree.Item {
	return &instance{i: i}
}

//
// executable Functions
//

func (inst *instance) Identifier() executableID {
	return pb.Dependency{
		ReplicaID:   inst.r,
		InstanceNum: inst.i,
	}
}

func (inst *instance) Dependencies() []executableID {
	deps := make([]executableID, 0, len(inst.deps))
	for dep := range inst.deps {
		deps = append(deps, dep)
	}
	return deps
}

// ExecutesBefore determines which of two instances execute first. The ordering
// is based on sequence numbers (lamport logical clocks), which break ties in
// strongly connected components. If the sequence numbers are also the same,
// then we break ties based on ReplicaID, because commands in the same SCC will
// always be from different replicas.
func (inst *instance) ExecutesBefore(b executable) bool {
	instB := b.(*instance)
	if seqA, seqB := inst.seq, instB.seq; seqA != seqB {
		return seqA < seqB
	}
	return inst.r < instB.r
}

func (inst *instance) Execute() {
	inst.p.execute(inst)
}

//
// State-Transitions
//

func (inst *instance) transitionToPreAccept() {
	inst.assertState(none)
	inst.state = preAccepted
	inst.broadcastPreAccept()
}

func (inst *instance) transitionToAccept() {
	inst.assertState(preAccepted)
	inst.state = accepted
	inst.broadcastAccept()
}

func (inst *instance) transitionToCommit() {
	inst.assertState(preAccepted, accepted)
	inst.state = committed
	inst.broadcastCommit()
	inst.prepareToExecute()
}

func (inst *instance) isStates(states ...instanceState) bool {
	cur := inst.state
	for _, s := range states {
		if s == cur {
			return true
		}
	}
	return false
}

func (inst *instance) assertState(valid ...instanceState) {
	if !inst.isStates(valid...) {
		inst.p.logger.Panicf("unexpected state %v; expected %v", inst.state, valid)
	}
}

// broadcastPreAccept broadcasts a PreAccept message to all other nodes.
func (inst *instance) broadcastPreAccept() {
	inst.broadcast(&pb.PreAccept{InstanceState: inst.instanceState()})
}

// broadcastAccept broadcasts an Accept message to all other nodes.
func (inst *instance) broadcastAccept() {
	inst.broadcast(&pb.Accept{InstanceState: inst.instanceStateWithoutCommand()})
}

// broadcastCommit broadcasts a Commit message to all other nodes.
func (inst *instance) broadcastCommit() {
	inst.broadcast(&pb.Commit{InstanceState: inst.instanceState()})
}

//
// Message Handlers
//

func (inst *instance) onPreAccept(pa *pb.PreAccept) {
	// Only handle if this is a new instance, and set the state to preAccepted.
	if !inst.isStates(none) {
		inst.p.logger.Debugf("ignoring PreAccept message while in state %v: %v", inst.state, pa)
		return
	}
	inst.state = preAccepted

	// Determine the local sequence number and deps for this command.
	maxLocalSeq, localDeps := inst.p.seqAndDepsForCommand(*pa.Command)

	// Record the command for the instance.
	inst.cmd = *pa.Command

	// The updated sequence number is set to the maximum of the local maximum
	// sequence number and the the PreAccept's sequence number
	inst.seq = pb.MaxSeqNum(pa.SeqNum, maxLocalSeq+1)

	// Determine the union of the local dependencies and the PreAccept's dependencies.
	depsUnion := localDeps
	for _, dep := range pa.Deps {
		depsUnion[dep] = struct{}{}
	}
	inst.deps = depsUnion

	// If the sequence number and the deps turn out to be the same as those in
	// the PreAccept message, reply with a simple PreAcceptOK message.
	if inst.seq == pa.SeqNum && len(inst.deps) == len(pa.Deps) {
		inst.reply(&pb.PreAcceptOK{})
		return
	}

	// Reply to PreAccept message with updated information.
	inst.reply(&pb.PreAcceptReply{
		UpdatedSeqNum: inst.seq,
		UpdatedDeps:   depSliceFromMap(depsUnion),
	})
}

// fastPathAvailable returns whether the fast path is still available, given
// (possibly zero) more PreAcceptReply messages.
func (inst *instance) fastPathAvailable() bool {
	return !inst.differentReplies
}

func (inst *instance) onPreAcceptOK(paOK *pb.PreAcceptOK) {
	if !inst.isStates(preAccepted) {
		inst.p.logger.Debugf("ignoring PreAcceptOK message while in state %v: %v", inst.state, paOK)
		return
	}

	inst.preAcceptReplies++
	inst.onEitherPreAcceptReply()
}

func (inst *instance) onPreAcceptReply(paReply *pb.PreAcceptReply) {
	if !inst.isStates(preAccepted) {
		inst.p.logger.Debugf("ignoring PreAcceptReply message while in state %v: %v", inst.state, paReply)
		return
	}

	// Update the instance state based on the PreAcceptReply.
	changed := inst.updateInstanceState(paReply.UpdatedSeqNum, paReply.UpdatedDeps)

	// Update whether we've ever seen any new information in PreAcceptReply messages.
	inst.differentReplies = inst.differentReplies || changed

	inst.preAcceptReplies++
	inst.onEitherPreAcceptReply()
}

func (inst *instance) onEitherPreAcceptReply() {
	replies := inst.preAcceptReplies + 1 // +1 for leader
	takeFastPath := !inst.differentReplies && inst.p.fastQuorum(replies)
	takeSlowPath := inst.p.quorum(replies)
	switch {
	case takeFastPath:
		inst.p.unregisterTimer(&inst.slowPathTimer)
		inst.transitionToCommit()
	case takeSlowPath:
		// We have enough replies to take the slow path, however we don't want to
		// take it immediately in-case it's possible to take the fast path instead.
		if !inst.fastPathAvailable() {
			// Since the fast path will never be available, take the slow path.
			inst.p.unregisterTimer(&inst.slowPathTimer)
			inst.transitionToAccept()
		} else if !inst.slowPathTimer.isSet() {
			// Delay for a few ticks before taking slow path to allow for the fast
			// path quorum to be achieved.
			inst.p.registerOneTimeTimer(&inst.slowPathTimer)
		} else {
			// Timer already set. This reply will help us get to the fast path.
		}
	}
}

func (inst *instance) onAccept(a *pb.Accept) {
	if !inst.isStates(none, preAccepted) {
		inst.p.logger.Debugf("ignoring Accept message while in state %v: %v", inst.state, a)
		return
	}

	inst.state = accepted
	inst.updateInstanceState(a.SeqNum, a.Deps)
	inst.reply(&pb.AcceptOK{})
}

func (inst *instance) onAcceptOK(aOK *pb.AcceptOK) {
	if !inst.isStates(accepted) {
		inst.p.logger.Debugf("ignoring AcceptOK message while in state %v: %v", inst.state, aOK)
		return
	}

	inst.acceptReplies++
	if inst.p.quorum(inst.acceptReplies + 1 /* +1 for leader */) {
		inst.transitionToCommit()
	}
}

func (inst *instance) onCommit(c *pb.Commit) {
	if !inst.isStates(none, preAccepted, accepted) {
		inst.p.logger.Debugf("ignoring Commit message while in state %v: %v", inst.state, c)
		return
	}

	inst.state = committed
	inst.cmd = *c.Command
	inst.updateInstanceState(c.SeqNum, c.Deps)
	inst.prepareToExecute()
}

//
// Utility Functions
//

func (inst *instance) instanceStateWithoutCommand() pb.InstanceState {
	return pb.InstanceState{
		SeqNum: inst.seq,
		Deps:   inst.depSlice(),
	}
}

func (inst *instance) instanceState() pb.InstanceState {
	is := inst.instanceStateWithoutCommand()
	is.Command = &inst.cmd
	return is
}

// updateInstanceState updates the instance with the new sequence number and the
// new dependencies. It returns whether the instance was changed.
func (inst *instance) updateInstanceState(newSeq pb.SeqNum, newDeps []pb.Dependency) bool {
	// Check whether this PreAccept reply is identical to our preAccept or if
	// the remote peer returned extra information that we weren't aware of. An
	// identical fast path quorum allows us to skip the Paxos-Accept phase.
	sameSeq := inst.seq == newSeq
	if !sameSeq {
		// newSeq will always be larger if it is updated, so this
		// is identical to:
		//   inst.seq = pb.MaxSeqNum(inst.seq, newSeq)
		inst.seq = newSeq
	}

	// Length check == equality check, because depsUnion was a union of remote
	// deps and local deps.
	sameDeps := len(newDeps) == len(inst.deps)
	if !sameDeps {
		// Merge remote deps into local deps.
		for _, dep := range newDeps {
			inst.deps[dep] = struct{}{}
		}
	}

	changed := !(sameSeq && sameDeps)
	return changed
}

// depSlice returns the instance's dependencies as a slice instead of a map.
func (inst *instance) depSlice() []pb.Dependency {
	return depSliceFromMap(inst.deps)
}

func depSliceFromMap(depsMap map[pb.Dependency]struct{}) []pb.Dependency {
	deps := make([]pb.Dependency, 0, len(depsMap))
	for dep := range depsMap {
		deps = append(deps, dep)
	}
	// Sort so that the order is deterministic.
	sort.Sort(pb.Dependencies(deps))
	return deps
}

func (inst *instance) prepareToExecute() {
	inst.p.prepareToExecute(inst)
}
