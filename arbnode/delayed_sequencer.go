// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbnode

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	flag "github.com/spf13/pflag"

	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/util/headerreader"
	"github.com/offchainlabs/nitro/util/stopwaiter"
)

type DelayedSequencer struct {
	stopwaiter.StopWaiter
	l1Reader                 *headerreader.HeaderReader
	bridge                   *DelayedBridge
	inbox                    *InboxTracker
	txStreamer               *TransactionStreamer
	coordinator              *SeqCoordinator
	waitingForFinalizedBlock uint64
	config                   DelayedSequencerConfigFetcher
}

type DelayedSequencerConfig struct {
	Enable              bool  `koanf:"enable" reload:"hot"`
	FinalizeDistance    int64 `koanf:"finalize-distance" reload:"hot"`
	RequireFullFinality bool  `koanf:"require-full-finality" reload:"hot"`
	UseMergeFinality    bool  `koanf:"use-merge-finality" reload:"hot"`
}

type DelayedSequencerConfigFetcher func() *DelayedSequencerConfig

func DelayedSequencerConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Bool(prefix+".enable", DefaultSeqCoordinatorConfig.Enable, "enable sequence coordinator")
	f.Int64(prefix+".finalize-distance", DefaultDelayedSequencerConfig.FinalizeDistance, "how many blocks in the past L1 block is considered final (ignored when using Merge finality)")
	f.Bool(prefix+".require-full-finality", DefaultDelayedSequencerConfig.RequireFullFinality, "whether to wait for full finality before sequencing delayed messages")
	f.Bool(prefix+".use-merge-finality", DefaultDelayedSequencerConfig.UseMergeFinality, "whether to use The Merge's notion of finality before sequencing delayed messages")
}

var DefaultDelayedSequencerConfig = DelayedSequencerConfig{
	Enable:              false,
	FinalizeDistance:    20,
	RequireFullFinality: true,
	UseMergeFinality:    true,
}

var TestDelayedSequencerConfig = DelayedSequencerConfig{
	Enable:              true,
	FinalizeDistance:    20,
	RequireFullFinality: true,
	UseMergeFinality:    true,
}

func NewDelayedSequencer(l1Reader *headerreader.HeaderReader, reader *InboxReader, txStreamer *TransactionStreamer, coordinator *SeqCoordinator, config DelayedSequencerConfigFetcher) (*DelayedSequencer, error) {
	return &DelayedSequencer{
		l1Reader:    l1Reader,
		bridge:      reader.DelayedBridge(),
		inbox:       reader.Tracker(),
		coordinator: coordinator,
		txStreamer:  txStreamer,
		config:      config,
	}, nil
}

func (d *DelayedSequencer) getDelayedMessagesRead() (uint64, error) {
	pos, err := d.txStreamer.GetMessageCount()
	if err != nil || pos == 0 {
		return 0, err
	}
	lastMsg, err := d.txStreamer.GetMessage(pos - 1)
	if err != nil {
		return 0, err
	}
	return lastMsg.DelayedMessagesRead, nil
}

func (d *DelayedSequencer) update(ctx context.Context, lastBlockHeader *types.Header) error {
	if d.coordinator != nil && !d.coordinator.CurrentlyChosen() {
		return nil
	}

	config := d.config()
	if !config.Enable {
		return nil
	}

	currentHeader, err := d.l1Reader.LastHeader(ctx)
	if err != nil {
		return err
	}
	if currentHeader == nil {
		return nil
	}

	var finalized uint64
	if config.UseMergeFinality && currentHeader.Difficulty.Sign() == 0 {
		if config.RequireFullFinality {
			finalized, err = d.l1Reader.LatestFinalizedBlockNr(ctx)
		} else {
			finalized, err = d.l1Reader.LatestSafeBlockNr(ctx)
		}
		if err != nil {
			return err
		}
	} else {
		currentNum := currentHeader.Number.Int64()
		if currentNum < config.FinalizeDistance {
			return nil
		}
		finalized = uint64(currentNum - config.FinalizeDistance)
	}

	if d.waitingForFinalizedBlock > finalized {
		return nil
	}

	// Unless we find an unfinalized message (which sets waitingForBlock),
	// we won't find a new finalized message until FinalizeDistance blocks in the future.
	d.waitingForFinalizedBlock = lastBlockHeader.Number.Uint64() + 1

	dbDelayedCount, err := d.inbox.GetDelayedCount()
	if err != nil {
		return err
	}
	startPos, err := d.getDelayedMessagesRead()
	if err != nil {
		return err
	}

	// Retrieve all finalized delayed messages
	pos := startPos
	var lastDelayedAcc common.Hash
	var messages []*arbos.L1IncomingMessage
	for pos < dbDelayedCount {
		msg, acc, err := d.inbox.GetDelayedMessageAndAccumulator(pos)
		if err != nil {
			return err
		}
		if msg.Header.BlockNumber > finalized {
			// Message isn't finalized yet; stop here
			d.waitingForFinalizedBlock = msg.Header.BlockNumber
			break
		}
		if lastDelayedAcc != (common.Hash{}) {
			// Ensure that there hasn't been a reorg and this message follows the last
			fullMsg := DelayedInboxMessage{
				BeforeInboxAcc: lastDelayedAcc,
				Message:        msg,
			}
			if fullMsg.AfterInboxAcc() != acc {
				return errors.New("delayed message accumulator mismatch while sequencing")
			}
		}
		lastDelayedAcc = acc
		messages = append(messages, msg)
		pos++
	}

	// Sequence the delayed messages, if any
	if len(messages) > 0 {
		delayedBridgeAcc, err := d.bridge.GetAccumulator(ctx, pos-1, new(big.Int).SetUint64(finalized))
		if err != nil {
			return err
		}
		if delayedBridgeAcc != lastDelayedAcc {
			// Probably a reorg that hasn't been picked up by the inbox reader
			return fmt.Errorf("inbox reader at delayed message %v db accumulator %v doesn't match delayed bridge accumulator %v at L1 block %v", pos-1, lastDelayedAcc, delayedBridgeAcc, finalized)
		}

		err = d.txStreamer.SequenceDelayedMessages(ctx, messages, startPos)
		if err != nil {
			return err
		}
		log.Info("DelayedSequencer: Sequenced", "msgnum", len(messages), "startpos", startPos)
	}

	return nil
}

func (d *DelayedSequencer) run(ctx context.Context) {
	headerChan, cancel := d.l1Reader.Subscribe(false)
	defer cancel()

	for {
		select {
		case nextHeader, ok := <-headerChan:
			if !ok {
				log.Info("delayed sequencer: header channel close")
				return
			}
			if err := d.update(ctx, nextHeader); err != nil {
				log.Error("Delayed sequencer error", "err", err)
			}
		case <-ctx.Done():
			log.Info("delayed sequencer: context done", "err", ctx.Err())
			return
		}
	}
}

func (d *DelayedSequencer) Start(ctxIn context.Context) {
	d.StopWaiter.Start(ctxIn, d)
	d.LaunchThread(d.run)
}
