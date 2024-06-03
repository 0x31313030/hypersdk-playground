// Copyright (C) 2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package actions

import (
	"context"

	"github.com/ava-labs/avalanchego/ids"

	"github.com/ava-labs/hypersdk/chain"
	"github.com/ava-labs/hypersdk/codec"
	"github.com/ava-labs/hypersdk/consts"
	"github.com/ava-labs/hypersdk/examples/morpheusvm/storage"
	"github.com/ava-labs/hypersdk/state"

	mconsts "github.com/ava-labs/hypersdk/examples/morpheusvm/consts"
)

var _ chain.Action = (*Burn)(nil)

type Burn struct {
	// Amount are transferred to [To].
	Value uint64 `json:"value"`
}

func (*Burn) GetTypeID() uint8 {
	return mconsts.BurnId
}

func (t *Burn) StateKeys(actor codec.Address, _ ids.ID) state.Keys {
	return state.Keys{
		string(storage.BalanceKey(actor)): state.Read | state.Write,
	}
}

func (*Burn) StateKeysMaxChunks() []uint16 {
	return []uint16{storage.BalanceChunks}
}

func (t *Burn) Execute(
	ctx context.Context,
	_ chain.Rules,
	mu state.Mutable,
	_ int64,
	actor codec.Address,
	_ ids.ID,
) ([][]byte, error) {
	if t.Value == 0 {
		return nil, ErrOutputValueZero
	}
	if err := storage.SubBalance(ctx, mu, actor, t.Value); err != nil {
		return nil, err
	}
	return nil, nil
}

func (*Burn) ComputeUnits(chain.Rules) uint64 {
	return TransferComputeUnits
}

func (*Burn) Size() int {
	return consts.Uint64Len
}

func (t *Burn) Marshal(p *codec.Packer) {
	p.PackUint64(t.Value)
}

func UnmarshalBurn(p *codec.Packer) (chain.Action, error) {
	var burn Burn
	burn.Value = p.UnpackUint64(true)
	if err := p.Err(); err != nil {
		return nil, err
	}
	return &burn, nil
}

func (*Burn) ValidRange(chain.Rules) (int64, int64) {
	// Returning -1, -1 means that the action is always valid.
	return -1, -1
}
