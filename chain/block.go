// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package chain

import (
	"context"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/x/merkledb"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/ava-labs/hypersdk/codec"
	"github.com/ava-labs/hypersdk/utils"
	"github.com/ava-labs/hypersdk/window"
	"github.com/ava-labs/hypersdk/workers"
)

var (
	_ snowman.Block      = &StatelessBlock{}
	_ block.StateSummary = &SyncableBlock{}
)

const (
	// AvalancheGo imposes a limit of 2 MiB on the network, so we limit at
	// 2 MiB - ProposerVM header - Protobuf encoding overhead (we assume this is
	// no more than 50 KiB of overhead but is likely much less)
	NetworkSizeLimit = 2_044_723 // 1.95 MiB
)

type StatefulBlock struct {
	Prnt   ids.ID `json:"parent"`
	Tmstmp int64  `json:"timestamp"`
	Hght   uint64 `json:"height"`

	UnitPrice  uint64        `json:"unitPrice"`
	UnitWindow window.Window `json:"unitWindow"`

	BlockCost   uint64        `json:"blockCost"`
	BlockWindow window.Window `json:"blockWindow"`

	Txs []*Transaction `json:"txs"`

	StateRoot     ids.ID `json:"stateRoot"`
	UnitsConsumed uint64 `json:"unitsConsumed"`
	SurplusFee    uint64 `json:"surplusFee"`
}

func NewGenesisBlock(root ids.ID, minUnit uint64, minBlock uint64) *StatefulBlock {
	return &StatefulBlock{
		UnitPrice:  minUnit,
		UnitWindow: window.Window{},

		BlockCost:   minBlock,
		BlockWindow: window.Window{},

		StateRoot: root,
	}
}

// Stateless is defined separately from "Block"
// in case external packages needs use the stateful block
// without mocking VM or parent block
type StatelessBlock struct {
	*StatefulBlock `json:"block"`

	id     ids.ID
	st     choices.Status
	t      time.Time
	bytes  []byte
	txsSet map[ids.ID]struct{}

	results []*Result

	vm    VM
	state merkledb.TrieView

	sigJob *workers.Job
}

func NewBlock(ectx *ExecutionContext, vm VM, parent snowman.Block, tmstp int64) *StatelessBlock {
	return &StatelessBlock{
		StatefulBlock: &StatefulBlock{
			Prnt:   parent.ID(),
			Tmstmp: tmstp,
			Hght:   parent.Height() + 1,

			UnitPrice:  ectx.NextUnitPrice,
			UnitWindow: ectx.NextUnitWindow,

			BlockCost:   ectx.NextBlockCost,
			BlockWindow: ectx.NextBlockWindow,
		},
		vm: vm,
		st: choices.Processing,
	}
}

func ParseBlock(
	ctx context.Context,
	source []byte,
	status choices.Status,
	vm VM,
) (*StatelessBlock, error) {
	ctx, span := vm.Tracer().Start(ctx, "chain.ParseBlock")
	defer span.End()

	blk, err := UnmarshalBlock(source, vm)
	if err != nil {
		return nil, err
	}
	// Not guaranteed that a parsed block is verified
	return ParseStatefulBlock(ctx, blk, source, status, vm)
}

func (b *StatelessBlock) populateTxs(ctx context.Context, verifySigs bool) error {
	vm := b.vm
	ctx, span := vm.Tracer().Start(ctx, "StatelessBlock.populateTxs")
	defer span.End()

	// Setup signature verification job
	job, err := b.vm.Workers().NewJob(len(b.Txs))
	if err != nil {
		return err
	}
	b.sigJob = job

	// Process transactions
	_, sspan := vm.Tracer().Start(ctx, "StatelessBlock.verifySignatures")
	actionRegistry, authRegistry := b.vm.Registry()
	b.txsSet = map[ids.ID]struct{}{}
	for _, tx := range b.Txs {
		sigTask, err := tx.Init(ctx, actionRegistry, authRegistry)
		if err != nil {
			return err
		}
		if verifySigs {
			b.sigJob.Go(sigTask)
		}
		if _, ok := b.txsSet[tx.ID()]; ok {
			return ErrDuplicateTx
		}
		b.txsSet[tx.ID()] = struct{}{}
	}
	b.sigJob.Done(func() { sspan.End() })
	return nil
}

func ParseStatefulBlock(
	ctx context.Context,
	blk *StatefulBlock,
	source []byte,
	status choices.Status,
	vm VM,
) (*StatelessBlock, error) {
	ctx, span := vm.Tracer().Start(ctx, "chain.ParseStatefulBlock")
	defer span.End()

	// Perform basic correctness checks before doing any expensive work
	if blk.Hght > 0 { // skip genesis
		if blk.Tmstmp >= time.Now().Add(FutureBound).Unix() {
			return nil, ErrTimestampTooLate
		}
		if len(blk.Txs) == 0 {
			return nil, ErrNoTxs
		}
		r := vm.Rules(blk.Tmstmp)
		if len(blk.Txs) > r.GetMaxBlockTxs() {
			return nil, ErrBlockTooBig
		}
	}

	if len(source) == 0 {
		actionRegistry, authRegistry := vm.Registry()
		nsource, err := blk.Marshal(actionRegistry, authRegistry)
		if err != nil {
			return nil, err
		}
		source = nsource
	}
	b := &StatelessBlock{
		StatefulBlock: blk,
		t:             time.Unix(blk.Tmstmp, 0),
		bytes:         source,
		st:            status,
		vm:            vm,
		id:            utils.ToID(source),
	}

	// If we are parsing an older block, it will not be re-executed and should
	// not be tracked as a parsed block
	lastAccepted := b.vm.LastAcceptedBlock()
	if lastAccepted == nil || b.Hght <= lastAccepted.Hght { // nil when parsing genesis
		return b, nil
	}

	// Populate hashes and tx set
	return b, b.populateTxs(ctx, true)
}

// [init] is used during block building and testing
// TODO: remove init
func (b *StatelessBlock) init(ctx context.Context, results []*Result, validateSigs bool) error {
	ctx, span := b.vm.Tracer().Start(ctx, "StatelessBlock.init")
	defer span.End()

	blk, err := b.StatefulBlock.Marshal(b.vm.Registry())
	if err != nil {
		return err
	}
	b.bytes = blk
	b.id = utils.ToID(b.bytes)
	b.t = time.Unix(b.StatefulBlock.Tmstmp, 0)
	b.results = results

	// Populate hashes and tx set
	return b.populateTxs(ctx, validateSigs)
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) ID() ids.ID { return b.id }

// Must handle re-reverification...
//
// Invariants:
// Accepted / Rejected blocks should never have Verify called on them.
// Blocks that were verified (and returned nil) with Verify will not have verify called again.
// Blocks that were verified with VerifyWithContext may have verify called multiple times.
//
// When this may be called:
//  1. [Verify]
//  2. If the parent state is missing when verifying (dynamic state sync)
//  3. If the state of a block we are accepting is missing (finishing dynamic
//     state sync)
func (b *StatelessBlock) verify(ctx context.Context) (merkledb.TrieView, error) {
	log := b.vm.Logger()
	built := len(b.results) > 0
	r := b.vm.Rules(b.Tmstmp)

	// Perform basic correctness checks before doing any expensive work
	if b.Timestamp().Unix() >= time.Now().Add(FutureBound).Unix() {
		return nil, ErrTimestampTooLate
	}
	if len(b.Txs) == 0 {
		return nil, ErrNoTxs
	}
	if len(b.Txs) > r.GetMaxBlockTxs() {
		return nil, ErrBlockTooBig
	}

	// Verify parent is verified and available
	parent, err := b.vm.GetStatelessBlock(ctx, b.Prnt)
	if err != nil {
		log.Debug("could not get parent", zap.Stringer("id", b.Prnt))
		return nil, err
	}
	if b.Timestamp().Unix() < parent.Timestamp().Unix() {
		return nil, ErrTimestampTooEarly
	}

	// Ensure tx cannot be replayed
	//
	// Before node is considered ready (emap is fully populated), this may return
	// false when other validators think it is true.
	oldestAllowed := b.Tmstmp - r.GetValidityWindow()
	if oldestAllowed < 0 {
		// Can occur if verifying genesis
		oldestAllowed = 0
	}
	dup, err := parent.IsRepeat(ctx, oldestAllowed, b.Txs)
	if err != nil {
		return nil, err
	}
	if dup {
		return nil, fmt.Errorf("%w: duplicate in ancestry", ErrDuplicateTx)
	}

	ectx, err := GenerateExecutionContext(ctx, b.Tmstmp, parent, b.vm.Tracer(), r)
	if err != nil {
		return nil, err
	}
	if b.UnitPrice != ectx.NextUnitPrice {
		return nil, ErrInvalidUnitPrice
	}
	if b.UnitWindow != ectx.NextUnitWindow {
		return nil, ErrInvalidUnitWindow
	}
	if b.BlockCost != ectx.NextBlockCost {
		return nil, ErrInvalidBlockCost
	}
	if b.BlockWindow != ectx.NextBlockWindow {
		return nil, ErrInvalidBlockWindow
	}
	log.Info(
		"verify context",
		zap.Uint64("height", b.Hght),
		zap.Uint64("unit price", b.UnitPrice),
		zap.Uint64("block cost", b.BlockCost),
	)

	// Fetch parent state
	//
	// This function may verify the parent if it is not yet verified.
	state, err := parent.childState(ctx, len(b.Txs)*2)
	if err != nil {
		return nil, err
	}

	// Optimisticaly fetch state
	processor := NewProcessor(b.vm.Tracer(), b)
	processor.Prefetch(ctx, state)

	// Process new transactions
	unitsConsumed, surplusFee, results, err := processor.Execute(ctx, ectx, r)
	if err != nil {
		return nil, err
	}
	b.results = results
	if b.UnitsConsumed != unitsConsumed {
		return nil, fmt.Errorf(
			"%w: required=%d found=%d",
			ErrInvalidUnitsConsumed,
			unitsConsumed,
			b.UnitsConsumed,
		)
	}

	// Ensure enough fee is paid to compensate for block production speed
	if b.SurplusFee != surplusFee {
		return nil, fmt.Errorf(
			"%w: required=%d found=%d",
			ErrInvalidSurplus,
			b.SurplusFee,
			surplusFee,
		)
	}
	requiredSurplus := b.UnitPrice * b.BlockCost
	if surplusFee < requiredSurplus {
		return nil, fmt.Errorf(
			"%w: required=%d found=%d",
			ErrInsufficientSurplus,
			requiredSurplus,
			surplusFee,
		)
	}

	// Compute state root
	// TODO: consider adding the parent root or height here to ensure state roots
	// are never repeated
	computedRoot, err := state.GetMerkleRoot(ctx)
	if err != nil {
		return nil, err
	}
	if b.StateRoot != computedRoot {
		return nil, fmt.Errorf(
			"%w: expected=%s found=%s",
			ErrStateRootMismatch,
			computedRoot,
			b.StateRoot,
		)
	}

	// Ensure signatures are verified
	if !built { // don't need to verify sigs if built
		_, sspan := b.vm.Tracer().Start(ctx, "StatelessBlock.Verify.WaitSignatures")
		defer sspan.End()
		if err := b.sigJob.Wait(); err != nil {
			return nil, err
		}
	}
	return state, nil
}

// implements "snowman.Block"
func (b *StatelessBlock) Verify(ctx context.Context) error {
	stateReady := b.vm.StateReady()
	ctx, span := b.vm.Tracer().Start(
		ctx, "StatelessBlock.Verify",
		oteltrace.WithAttributes(
			attribute.Int("txs", len(b.Txs)),
			attribute.Bool("stateReady", stateReady),
		),
	)
	defer span.End()

	// If the state of the accepted tip has been fully fetched, it is safe to
	// verify any block.
	if stateReady {
		// Parent may not be processed when we verify this block so [verify] may
		// recursively compute missing state.
		state, err := b.verify(ctx)
		if err != nil {
			return err
		}
		b.state = state
	}
	// NOTE: mempool is modified by VM handler
	b.vm.Verified(ctx, b)
	return nil
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) Accept(ctx context.Context) error {
	ctx, span := b.vm.Tracer().Start(ctx, "StatelessBlock.Accept")
	defer span.End()

	// Consider verifying the a block if it is not processed and we are no longer
	// syncing.
	if !b.Processed() {
		// The state of this block was not calculated during the call to
		// [StatelessBlock.Verify]. This is because the VM was state syncing
		// and did not have the state necessary to verify the block.
		updated, err := b.vm.UpdateSyncTarget(b)
		if err != nil {
			return err
		}
		if updated {
			b.vm.Logger().
				Info("updated state sync target", zap.Stringer("id", b.ID()), zap.Stringer("root", b.StateRoot))
			return nil // the sync is still ongoing
		}
		b.vm.Logger().
			Info("verifying unprocessed block in accept", zap.Stringer("id", b.ID()), zap.Stringer("root", b.StateRoot))
		// This check handles the case where blocks were not
		// verified during state sync (stopped syncing with a processing block).
		//
		// If state sync completes before accept is called
		// then we need to rebuild it here.
		state, err := b.verify(ctx)
		if err != nil {
			return err
		}
		b.state = state
	}

	// Commit state if we don't return before here (would happen if we are still
	// syncing)
	if err := b.state.Commit(ctx); err != nil {
		return err
	}

	// Set last accepted block
	return b.SetLastAccepted(ctx)
}

// SetLastAccepted is called during [Accept] and at the start and end of state
// sync.
func (b *StatelessBlock) SetLastAccepted(ctx context.Context) error {
	if err := b.vm.SetLastAccepted(b); err != nil {
		return err
	}
	b.st = choices.Accepted
	b.txsSet = nil // only used for replay protection when processing

	// [Accepted] will set in-memory variables needed to ensure we don't resync
	// all blocks when state sync finishes
	//
	// Note: We will not call [b.vm.Verified] before accepting during state sync
	b.vm.Accepted(ctx, b)
	return nil
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) Reject(ctx context.Context) error {
	ctx, span := b.vm.Tracer().Start(ctx, "StatelessBlock.Reject")
	defer span.End()

	b.st = choices.Rejected
	b.vm.Rejected(ctx, b)
	return nil
}

// implements "snowman.Block.choices.Decidable"
func (b *StatelessBlock) Status() choices.Status { return b.st }

// implements "snowman.Block"
func (b *StatelessBlock) Parent() ids.ID { return b.StatefulBlock.Prnt }

// implements "snowman.Block"
func (b *StatelessBlock) Bytes() []byte { return b.bytes }

// implements "snowman.Block"
func (b *StatelessBlock) Height() uint64 { return b.StatefulBlock.Hght }

// implements "snowman.Block"
func (b *StatelessBlock) Timestamp() time.Time { return b.t }

// State is used to verify txs in the mempool. It should never be written to.
//
// TODO: we should modify the interface here to only allow read-like messages
func (b *StatelessBlock) State() (Database, error) {
	if b.st == choices.Accepted {
		return b.vm.State()
	}
	if b.Processed() {
		return b.state, nil
	}
	return nil, ErrBlockNotProcessed
}

// Used to determine if should notify listeners and/or pass to controller
func (b *StatelessBlock) Processed() bool {
	return b.state != nil
}

// We assume this will only be called once we are done syncing, so it is safe
// to assume we will eventually get to a block with state.
func (b *StatelessBlock) childState(
	ctx context.Context,
	estimatedChanges int,
) (merkledb.TrieView, error) {
	ctx, span := b.vm.Tracer().Start(ctx, "StatelessBlock.childState")
	defer span.End()

	// Return committed state if block is accepted or this is genesis.
	if b.st == choices.Accepted || b.Hght == 0 /* genesis */ {
		state, err := b.vm.State()
		if err != nil {
			return nil, err
		}
		return state.NewPreallocatedView(ctx, estimatedChanges)
	}

	// Process block if not yet processed and not yet accepted.
	if !b.Processed() {
		b.vm.Logger().
			Info("verifying parent when childState requested", zap.Uint64("height", b.Hght))
		state, err := b.verify(ctx)
		if err != nil {
			return nil, err
		}
		b.state = state
	}
	return b.state.NewPreallocatedView(ctx, estimatedChanges)
}

func (b *StatelessBlock) IsRepeat(
	ctx context.Context,
	oldestAllowed int64,
	txs []*Transaction,
) (bool, error) {
	ctx, span := b.vm.Tracer().Start(ctx, "StatelessBlock.IsRepeat")
	defer span.End()

	// Early exit if we are already back at least [ValidityWindow]
	if b.Tmstmp < oldestAllowed {
		return false, nil
	}

	// If we are at an accepted block or genesis, we can use the emap on the VM
	// instead of checking each block
	if b.st == choices.Accepted || b.Hght == 0 /* genesis */ {
		return b.vm.IsRepeat(ctx, txs), nil
	}

	// Check if block contains any overlapping txs
	for _, tx := range txs {
		if _, ok := b.txsSet[tx.ID()]; ok {
			return true, nil
		}
	}
	prnt, err := b.vm.GetStatelessBlock(ctx, b.Prnt)
	if err != nil {
		return false, err
	}
	return prnt.IsRepeat(ctx, oldestAllowed, txs)
}

func (b *StatelessBlock) GetTxs() []*Transaction {
	return b.Txs
}

func (b *StatelessBlock) GetTimestamp() int64 {
	return b.Tmstmp
}

func (b *StatelessBlock) GetUnitPrice() uint64 {
	return b.UnitPrice
}

func (b *StatelessBlock) Results() []*Result {
	return b.results
}

func (b *StatefulBlock) Marshal(
	actionRegistry ActionRegistry,
	authRegistry AuthRegistry,
) ([]byte, error) {
	p := codec.NewWriter(NetworkSizeLimit)

	p.PackID(b.Prnt)
	p.PackInt64(b.Tmstmp)
	p.PackUint64(b.Hght)

	p.PackUint64(b.UnitPrice)
	p.PackWindow(b.UnitWindow)

	p.PackUint64(b.BlockCost)
	p.PackWindow(b.BlockWindow)

	p.PackInt(len(b.Txs))
	for _, tx := range b.Txs {
		if err := tx.Marshal(p, actionRegistry, authRegistry); err != nil {
			return nil, err
		}
	}

	p.PackID(b.StateRoot)
	p.PackUint64(b.UnitsConsumed)
	p.PackUint64(b.SurplusFee)
	return p.Bytes(), p.Err()
}

func UnmarshalBlock(raw []byte, parser Parser) (*StatefulBlock, error) {
	var (
		p = codec.NewReader(raw, NetworkSizeLimit)
		b StatefulBlock
	)

	p.UnpackID(false, &b.Prnt)
	b.Tmstmp = p.UnpackInt64(false)
	b.Hght = p.UnpackUint64(false)

	b.UnitPrice = p.UnpackUint64(false)
	p.UnpackWindow(&b.UnitWindow)

	b.BlockCost = p.UnpackUint64(false)
	p.UnpackWindow(&b.BlockWindow)
	if err := p.Err(); err != nil {
		// Check that header was parsed properly before unwrapping transactions
		return nil, err
	}

	// Parse transactions
	txCount := p.UnpackInt(false) // could be 0 in genesis
	actionRegistry, authRegistry := parser.Registry()
	b.Txs = []*Transaction{} // don't preallocate all to avoid DoS
	for i := 0; i < txCount; i++ {
		tx, err := UnmarshalTx(p, actionRegistry, authRegistry)
		if err != nil {
			return nil, err
		}
		b.Txs = append(b.Txs, tx)
	}

	p.UnpackID(false, &b.StateRoot)
	b.UnitsConsumed = p.UnpackUint64(false)
	b.SurplusFee = p.UnpackUint64(false)

	if !p.Empty() {
		// Ensure no leftover bytes
		return nil, ErrInvalidObject
	}
	return &b, p.Err()
}

type SyncableBlock struct {
	*StatelessBlock
}

func (sb *SyncableBlock) Accept(ctx context.Context) (block.StateSyncMode, error) {
	return sb.vm.AcceptedSyncableBlock(ctx, sb)
}

func NewSyncableBlock(sb *StatelessBlock) *SyncableBlock {
	return &SyncableBlock{sb}
}

func (sb *SyncableBlock) String() string {
	return fmt.Sprintf("%d:%s root=%s", sb.Height(), sb.ID(), sb.StateRoot)
}