// Copyright © 2019 Binance
//
// This file is part of Binance. The full Binance copyright notice, including
// terms governing use, modification, and redistribution, is contained in the
// file LICENSE at the root of the source code distribution tree.

package signing

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"strings"

	"github.com/binance-chain/tss-lib/common"
	"github.com/binance-chain/tss-lib/crypto"
	cmt "github.com/binance-chain/tss-lib/crypto/commitments"
	"github.com/binance-chain/tss-lib/crypto/mta"
	"github.com/binance-chain/tss-lib/ecdsa/keygen"
	_ "github.com/binance-chain/tss-lib/eddsa/signing" // has to be here otherwise 2021-01-19 18:52:06.820 TrusTSS[81825:3183626] failed to update session: {\"NSLocalizedDescription\":\"task signing, party {1,congip11pro_1216}, round 2: proto: cannot parse reserved wire type\"} fails
	"github.com/binance-chain/tss-lib/tss"
)

// Implements Party
// Implements Stringer
var _ tss.Party = (*LocalParty)(nil)
var _ fmt.Stringer = (*LocalParty)(nil)

type (
	LocalParty struct {
		*tss.BaseParty
		params *tss.Parameters

		keys keygen.LocalPartySaveData
		temp localTempData
		data SignatureData

		// outbound messaging
		out chan<- tss.Message
		end chan<- *SignatureData
	}

	localMessageStore struct {
		signRound1Message1s,
		signRound1Message2s,
		signRound2Messages,
		signRound3Messages,
		signRound4Messages,
		signRound5Messages,
		signRound6Messages,
		signRound7Messages []tss.ParsedMessage
	}

	localTempData struct {
		localMessageStore

		// temp data (thrown away after sign) / round 1
		m,
		wI,
		cAKI,
		rAKI,
		deltaI,
		sigmaI,
		gammaI *big.Int
		bigWs    []*crypto.ECPoint
		gammaIG  *crypto.ECPoint
		deCommit cmt.HashDeCommitment

		// round 2
		betas, // return value of Bob_mid
		c1JIs,
		c2JIs,
		vJIs []*big.Int // return value of Bob_mid_wc
		pI1JIs []*mta.ProofBob
		pI2JIs []*mta.ProofBobWC

		// round 3
		lI *big.Int

		// round 5
		bigGammaJs  []*crypto.ECPoint
		r5AbortData SignRound6Message_AbortData

		// round 6
		SignatureData_OneRoundData

		// round 7
		sI *big.Int
		rI,
		TI *crypto.ECPoint
		r7AbortData SignRound7Message_AbortData
	}
)

// Constructs a new ECDSA signing party. Note: msg may be left nil for one-round signing mode to only do the pre-processing steps.
func NewLocalParty(
	msg *big.Int,
	params *tss.Parameters,
	key keygen.LocalPartySaveData,
	out chan<- tss.Message,
	end chan<- *SignatureData,
) tss.Party {
	partyCount := len(params.Parties().IDs())
	p := &LocalParty{
		BaseParty: new(tss.BaseParty),
		params:    params,
		keys:      keygen.BuildLocalSaveDataSubset(key, params.Parties().IDs()),
		temp:      localTempData{},
		data:      SignatureData{},
		out:       out,
		end:       end,
	}
	// msgs init
	p.temp.signRound1Message1s = make([]tss.ParsedMessage, partyCount)
	p.temp.signRound1Message2s = make([]tss.ParsedMessage, partyCount)
	p.temp.signRound2Messages = make([]tss.ParsedMessage, partyCount)
	p.temp.signRound3Messages = make([]tss.ParsedMessage, partyCount)
	p.temp.signRound4Messages = make([]tss.ParsedMessage, partyCount)
	p.temp.signRound5Messages = make([]tss.ParsedMessage, partyCount)
	p.temp.signRound6Messages = make([]tss.ParsedMessage, partyCount)
	p.temp.signRound7Messages = make([]tss.ParsedMessage, partyCount)
	// temp data init
	p.temp.m = msg
	p.temp.bigWs = make([]*crypto.ECPoint, partyCount)
	p.temp.betas = make([]*big.Int, partyCount)
	p.temp.c1JIs = make([]*big.Int, partyCount)
	p.temp.c2JIs = make([]*big.Int, partyCount)
	p.temp.pI1JIs = make([]*mta.ProofBob, partyCount)
	p.temp.pI2JIs = make([]*mta.ProofBobWC, partyCount)
	p.temp.vJIs = make([]*big.Int, partyCount)
	p.temp.bigGammaJs = make([]*crypto.ECPoint, partyCount)
	p.temp.r5AbortData.AlphaIJ = make([][]byte, partyCount)
	p.temp.r5AbortData.BetaJI = make([][]byte, partyCount)
	return p
}

// Constructs a new ECDSA signing party for one-round signing. The final SignatureData struct will be a partial struct containing only the data for a final signing round (see the readme).
func NewLocalPartyWithOneRoundSign(
	params *tss.Parameters,
	key keygen.LocalPartySaveData,
	out chan<- tss.Message,
	end chan<- *SignatureData,
) tss.Party {
	return NewLocalParty(nil, params, key, out, end)
}

func (p *LocalParty) FirstRound() tss.Round {
	return newRound1(p.params, &p.keys, &p.data, &p.temp, p.out, p.end)
}

func (p *LocalParty) Start() *tss.Error {
	return tss.BaseStart(p, TaskName, func(round tss.Round) *tss.Error {
		round1, ok := round.(*round1)
		if !ok {
			return round.WrapError(errors.New("unable to Start(). party is in an unexpected round"))
		}
		if err := round1.prepare(); err != nil {
			return round.WrapError(err)
		}
		return nil
	})
}

func (p *LocalParty) Update(msg tss.ParsedMessage) (ok bool, err *tss.Error) {
	return tss.BaseUpdate(p, msg, TaskName)
}

func (p *LocalParty) UpdateFromBytes(wireBytes []byte, from *tss.PartyID, isBroadcast bool) (bool, *tss.Error) {
	msg, err := tss.ParseWireMessage(wireBytes, from, isBroadcast)
	if err != nil {
		return false, p.WrapError(err)
	}
	return p.Update(msg)
}

func (p *LocalParty) ValidateMessage(msg tss.ParsedMessage) (bool, *tss.Error) {
	if ok, err := p.BaseParty.ValidateMessage(msg); !ok || err != nil {
		return ok, err
	}
	// check that the message's "from index" will fit into the array
	if maxFromIdx := len(p.params.Parties().IDs()) - 1; maxFromIdx < msg.GetFrom().Index {
		return false, p.WrapError(fmt.Errorf("received msg with a sender index too great (%d <= %d)",
			maxFromIdx, msg.GetFrom().Index), msg.GetFrom())
	}
	return true, nil
}

func (p *LocalParty) StoreMessage(msg tss.ParsedMessage) (bool, *tss.Error) {
	// ValidateBasic is cheap; double-check the message here in case the public StoreMessage was called externally
	if ok, err := p.ValidateMessage(msg); !ok || err != nil {
		return ok, err
	}
	fromPIdx := msg.GetFrom().Index

	// switch/case is necessary to store any messages beyond current round
	// this does not handle message replays. we expect the caller to apply replay and spoofing protection.
	switch msg.Content().(type) {
	case *SignRound1Message1:
		p.temp.signRound1Message1s[fromPIdx] = msg
	case *SignRound1Message2:
		p.temp.signRound1Message2s[fromPIdx] = msg
	case *SignRound2Message:
		p.temp.signRound2Messages[fromPIdx] = msg
	case *SignRound3Message:
		p.temp.signRound3Messages[fromPIdx] = msg
	case *SignRound4Message:
		p.temp.signRound4Messages[fromPIdx] = msg
	case *SignRound5Message:
		p.temp.signRound5Messages[fromPIdx] = msg
	case *SignRound6Message:
		p.temp.signRound6Messages[fromPIdx] = msg
	case *SignRound7Message:
		p.temp.signRound7Messages[fromPIdx] = msg
	default: // unrecognised message, just ignore!
		common.Logger.Warnf("unrecognised message ignored: %v", msg)
		return false, nil
	}
	return true, nil
}

func (p *LocalParty) PartyID() *tss.PartyID {
	return p.params.PartyID()
}

func (p *LocalParty) String() string {
	return fmt.Sprintf("id: %s, %s", p.PartyID(), p.BaseParty.String())
}

func InTestCheck(flag string) bool {
	// depth of 10 is far more enough for testing environment
	pcs := make([]uintptr, 20)
	runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs)
	for {
		frame, more := frames.Next()
		if !more {
			break
		}
		if strings.Contains(frame.Function, flag) {
			return true
		}
	}
	return false
}
