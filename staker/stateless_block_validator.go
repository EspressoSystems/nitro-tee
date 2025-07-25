// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package staker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"

	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/daprovider"
	"github.com/offchainlabs/nitro/execution"
	"github.com/offchainlabs/nitro/util/rpcclient"
	"github.com/offchainlabs/nitro/validator"
	validatorclient "github.com/offchainlabs/nitro/validator/client"
	"github.com/offchainlabs/nitro/validator/client/redis"
	"github.com/offchainlabs/nitro/validator/server_api"
)

type StatelessBlockValidator struct {
	config *BlockValidatorConfig

	execSpawners     []validator.ExecutionSpawner
	boldExecSpawners []validator.BOLDExecutionSpawner
	redisValidator   *redis.ValidationClient

	recorder execution.ExecutionRecorder

	inboxReader          InboxReaderInterface
	inboxTracker         InboxTrackerInterface
	streamer             TransactionStreamerInterface
	db                   ethdb.Database
	dapReaders           []daprovider.Reader
	stack                *node.Node
	latestWasmModuleRoot common.Hash
}

type BlockValidatorRegistrer interface {
	SetBlockValidator(*BlockValidator)
}

type InboxTrackerInterface interface {
	BlockValidatorRegistrer
	GetDelayedMessageBytes(context.Context, uint64) ([]byte, error)
	GetBatchMessageCount(seqNum uint64) (arbutil.MessageIndex, error)
	GetBatchAcc(seqNum uint64) (common.Hash, error)
	GetBatchCount() (uint64, error)
	FindInboxBatchContainingMessage(pos arbutil.MessageIndex) (uint64, bool, error)
}

type TransactionStreamerInterface interface {
	BlockValidatorRegistrer
	GetProcessedMessageCount() (arbutil.MessageIndex, error)
	GetMessage(msgIdx arbutil.MessageIndex) (*arbostypes.MessageWithMetadata, error)
	ResultAtMessageIndex(msgIdx arbutil.MessageIndex) (*execution.MessageResult, error)
	PauseReorgs()
	ResumeReorgs()
	ChainConfig() *params.ChainConfig
}

type InboxReaderInterface interface {
	GetSequencerMessageBytes(ctx context.Context, seqNum uint64) ([]byte, common.Hash, error)
	GetFinalizedMsgCount(ctx context.Context) (arbutil.MessageIndex, error)
}

type GlobalStatePosition struct {
	BatchNumber uint64
	PosInBatch  uint64
}

// return the globalState position before and after processing message at the specified count
// batch-number must be provided by caller
func GlobalStatePositionsAtCount(
	tracker InboxTrackerInterface,
	count arbutil.MessageIndex,
	batch uint64,
) (GlobalStatePosition, GlobalStatePosition, error) {
	msgCountInBatch, err := tracker.GetBatchMessageCount(batch)
	if err != nil {
		return GlobalStatePosition{}, GlobalStatePosition{}, err
	}
	var firstInBatch arbutil.MessageIndex
	if batch > 0 {
		firstInBatch, err = tracker.GetBatchMessageCount(batch - 1)
		if err != nil {
			return GlobalStatePosition{}, GlobalStatePosition{}, err
		}
	}
	if msgCountInBatch < count {
		return GlobalStatePosition{}, GlobalStatePosition{}, fmt.Errorf("batch %d has msgCount %d, failed getting for %d", batch, msgCountInBatch-1, count)
	}
	if firstInBatch >= count {
		return GlobalStatePosition{}, GlobalStatePosition{}, fmt.Errorf("batch %d starts from %d, failed getting for %d", batch, firstInBatch, count)
	}
	posInBatch := uint64(count - firstInBatch - 1)
	startPos := GlobalStatePosition{batch, posInBatch}
	if msgCountInBatch == count {
		return startPos, GlobalStatePosition{batch + 1, 0}, nil
	}
	return startPos, GlobalStatePosition{batch, posInBatch + 1}, nil
}

type ValidationEntryStage uint32

const (
	Empty ValidationEntryStage = iota
	ReadyForRecord
	Ready
)

type FullBatchInfo struct {
	Number     uint64
	PostedData []byte
	MsgCount   arbutil.MessageIndex
	Preimages  map[arbutil.PreimageType]map[common.Hash][]byte
}

type validationEntry struct {
	Stage ValidationEntryStage
	// Valid since ReadyforRecord:
	Pos           arbutil.MessageIndex
	Start         validator.GoGlobalState
	End           validator.GoGlobalState
	HasDelayedMsg bool
	DelayedMsgNr  uint64
	ChainConfig   *params.ChainConfig
	// valid when created, removed after recording
	msg *arbostypes.MessageWithMetadata
	// Has batch when created - others could be added on record
	BatchInfo []validator.BatchInfo
	// Valid since Ready
	Preimages  daprovider.PreimagesMap
	UserWasms  state.UserWasms
	DelayedMsg []byte
}

func (e *validationEntry) ToInput(stylusArchs []ethdb.WasmTarget) (*validator.ValidationInput, error) {
	if e.Stage != Ready {
		return nil, errors.New("cannot create input from non-ready entry")
	}
	res := validator.ValidationInput{
		Id:            uint64(e.Pos),
		HasDelayedMsg: e.HasDelayedMsg,
		DelayedMsgNr:  e.DelayedMsgNr,
		Preimages:     e.Preimages,
		UserWasms:     make(map[ethdb.WasmTarget]map[common.Hash][]byte, len(e.UserWasms)),
		BatchInfo:     e.BatchInfo,
		DelayedMsg:    e.DelayedMsg,
		StartState:    e.Start,
		DebugChain:    e.ChainConfig.DebugMode(),
	}
	if len(stylusArchs) == 0 && len(e.UserWasms) > 0 {
		return nil, fmt.Errorf("stylus support is required")
	}
	for _, stylusArch := range stylusArchs {
		res.UserWasms[stylusArch] = make(map[common.Hash][]byte)
	}
	for hash, asmMap := range e.UserWasms {
		for _, stylusArch := range stylusArchs {
			if asm, exists := asmMap[stylusArch]; exists {
				res.UserWasms[stylusArch][hash] = asm
			} else {
				return nil, fmt.Errorf("stylusArch not supported by block validator: %v", stylusArch)
			}
		}
	}
	return &res, nil
}

func newValidationEntry(
	pos arbutil.MessageIndex,
	start validator.GoGlobalState,
	end validator.GoGlobalState,
	msg *arbostypes.MessageWithMetadata,
	fullBatchInfo *FullBatchInfo,
	prevBatches []validator.BatchInfo,
	prevDelayed uint64,
	chainConfig *params.ChainConfig,
) (*validationEntry, error) {
	preimages := make(map[arbutil.PreimageType]map[common.Hash][]byte)
	if fullBatchInfo == nil {
		return nil, fmt.Errorf("fullbatchInfo cannot be nil")
	}
	if fullBatchInfo.Number != start.Batch {
		return nil, fmt.Errorf("got wrong batch expected: %d got: %d", start.Batch, fullBatchInfo.Number)
	}
	valBatches := []validator.BatchInfo{
		{
			Number: fullBatchInfo.Number,
			Data:   fullBatchInfo.PostedData,
		},
	}
	valBatches = append(valBatches, prevBatches...)

	copyPreimagesInto(preimages, fullBatchInfo.Preimages)

	hasDelayed := false
	var delayedNum uint64
	if msg.DelayedMessagesRead == prevDelayed+1 {
		hasDelayed = true
		delayedNum = prevDelayed
	} else if msg.DelayedMessagesRead != prevDelayed {
		return nil, fmt.Errorf("illegal validation entry delayedMessage %d, previous %d", msg.DelayedMessagesRead, prevDelayed)
	}

	return &validationEntry{
		Stage:         ReadyForRecord,
		Pos:           pos,
		Start:         start,
		End:           end,
		HasDelayedMsg: hasDelayed,
		DelayedMsgNr:  delayedNum,
		msg:           msg,
		BatchInfo:     valBatches,
		ChainConfig:   chainConfig,
		Preimages:     preimages,
	}, nil
}

func NewStatelessBlockValidator(
	inboxReader InboxReaderInterface,
	inbox InboxTrackerInterface,
	streamer TransactionStreamerInterface,
	recorder execution.ExecutionRecorder,
	arbdb ethdb.Database,
	dapReaders []daprovider.Reader,
	config func() *BlockValidatorConfig,
	stack *node.Node,
	latestWasmModuleRoot common.Hash,
) (*StatelessBlockValidator, error) {
	var executionSpawners []validator.ExecutionSpawner
	var boldExecutionSpawners []validator.BOLDExecutionSpawner
	var redisValClient *redis.ValidationClient

	if config().RedisValidationClientConfig.Enabled() {
		var err error
		redisValClient, err = redis.NewValidationClient(&config().RedisValidationClientConfig)
		if err != nil {
			return nil, fmt.Errorf("creating new redis validation client: %w", err)
		}
	}
	configs := config().ValidationServerConfigs
	for i := range configs {
		i := i
		confFetcher := func() *rpcclient.ClientConfig { return &config().ValidationServerConfigs[i] }
		executionSpawner := validatorclient.NewExecutionClient(confFetcher, stack)
		executionSpawners = append(executionSpawners, executionSpawner)
		boldExecutionSpawners = append(boldExecutionSpawners, validatorclient.NewBOLDExecutionClient(executionSpawner))
	}

	if len(executionSpawners) == 0 {
		return nil, errors.New("no enabled execution servers")
	}

	if latestWasmModuleRoot == (common.Hash{}) {
		return nil, errors.New("latestWasmModuleRoot not set")
	}

	return &StatelessBlockValidator{
		config:               config(),
		recorder:             recorder,
		redisValidator:       redisValClient,
		inboxReader:          inboxReader,
		inboxTracker:         inbox,
		streamer:             streamer,
		db:                   arbdb,
		dapReaders:           dapReaders,
		execSpawners:         executionSpawners,
		boldExecSpawners:     boldExecutionSpawners,
		stack:                stack,
		latestWasmModuleRoot: latestWasmModuleRoot,
	}, nil
}

func (v *StatelessBlockValidator) readPostedBatch(ctx context.Context, batchNum uint64) ([]byte, error) {
	batchCount, err := v.inboxTracker.GetBatchCount()
	if err != nil {
		return nil, err
	}
	if batchCount <= batchNum {
		return nil, fmt.Errorf("batch not found: %d", batchNum)
	}
	postedData, _, err := v.inboxReader.GetSequencerMessageBytes(ctx, batchNum)
	return postedData, err
}

func (v *StatelessBlockValidator) ExecutionSpawners() []validator.ExecutionSpawner {
	return v.execSpawners
}

func (v *StatelessBlockValidator) BOLDExecutionSpawners() []validator.BOLDExecutionSpawner {
	return v.boldExecSpawners
}

func (v *StatelessBlockValidator) readFullBatch(ctx context.Context, batchNum uint64) (bool, *FullBatchInfo, error) {
	batchCount, err := v.inboxTracker.GetBatchCount()
	if err != nil {
		return false, nil, err
	}
	if batchCount <= batchNum {
		return false, nil, nil
	}
	batchMsgCount, err := v.inboxTracker.GetBatchMessageCount(batchNum)
	if err != nil {
		return false, nil, err
	}
	postedData, batchBlockHash, err := v.inboxReader.GetSequencerMessageBytes(ctx, batchNum)
	if err != nil {
		return false, nil, err
	}
	preimages := make(daprovider.PreimagesMap)
	if len(postedData) > 40 {
		foundDA := false
		for _, dapReader := range v.dapReaders {
			if dapReader != nil && dapReader.IsValidHeaderByte(ctx, postedData[40]) {
				var err error
				var preimagesRecorded daprovider.PreimagesMap
				_, preimagesRecorded, err = dapReader.RecoverPayloadFromBatch(ctx, batchNum, batchBlockHash, postedData, preimages, true)
				if err != nil {
					// Matches the way keyset validation was done inside DAS readers i.e logging the error
					//  But other daproviders might just want to return the error
					if strings.Contains(err.Error(), daprovider.ErrSeqMsgValidation.Error()) && daprovider.IsDASMessageHeaderByte(postedData[40]) {
						log.Error(err.Error())
					} else {
						return false, nil, err
					}
				} else {
					preimages = preimagesRecorded
				}
				foundDA = true
				break
			}
		}
		if !foundDA {
			if daprovider.IsDASMessageHeaderByte(postedData[40]) {
				log.Error("No DAS Reader configured, but sequencer message found with DAS header")
			}
		}
	}
	fullInfo := FullBatchInfo{
		Number:     batchNum,
		PostedData: postedData,
		MsgCount:   batchMsgCount,
		Preimages:  preimages,
	}
	return true, &fullInfo, nil
}

func copyPreimagesInto(dest, source map[arbutil.PreimageType]map[common.Hash][]byte) {
	for piType, piMap := range source {
		if dest[piType] == nil {
			dest[piType] = make(map[common.Hash][]byte, len(piMap))
		}
		for hash, preimage := range piMap {
			dest[piType][hash] = preimage
		}
	}
}

func (v *StatelessBlockValidator) ValidationEntryRecord(ctx context.Context, e *validationEntry) error {
	if e.Stage != ReadyForRecord {
		return fmt.Errorf("validation entry should be ReadyForRecord, is: %v", e.Stage)
	}
	if e.Pos != 0 {
		recording, err := v.recorder.RecordBlockCreation(ctx, e.Pos, e.msg)
		if err != nil {
			return err
		}
		if recording.BlockHash != e.End.BlockHash {
			return fmt.Errorf("recording failed: pos %d, hash expected %v, got %v", e.Pos, e.End.BlockHash, recording.BlockHash)
		}
		if recording.Preimages != nil {
			recordingPreimages := map[arbutil.PreimageType]map[common.Hash][]byte{
				arbutil.Keccak256PreimageType: recording.Preimages,
			}
			copyPreimagesInto(e.Preimages, recordingPreimages)
		}
		e.UserWasms = recording.UserWasms
	}
	if e.HasDelayedMsg {
		delayedMsg, err := v.inboxTracker.GetDelayedMessageBytes(ctx, e.DelayedMsgNr)
		if err != nil {
			log.Error(
				"error while trying to read delayed msg for proving",
				"err", err, "seq", e.DelayedMsgNr, "pos", e.Pos,
			)
			return fmt.Errorf("error while trying to read delayed msg for proving: %w", err)
		}
		e.DelayedMsg = delayedMsg
	}
	e.msg = nil // no longer needed
	e.Stage = Ready
	return nil
}

func BuildGlobalState(res execution.MessageResult, pos GlobalStatePosition) validator.GoGlobalState {
	return validator.GoGlobalState{
		BlockHash:  res.BlockHash,
		SendRoot:   res.SendRoot,
		Batch:      pos.BatchNumber,
		PosInBatch: pos.PosInBatch,
	}
}

// return the globalState position before and after processing message at the specified count
func (v *StatelessBlockValidator) GlobalStatePositionsAtCount(count arbutil.MessageIndex) (GlobalStatePosition, GlobalStatePosition, error) {
	if count == 0 {
		return GlobalStatePosition{}, GlobalStatePosition{}, errors.New("no initial state for count==0")
	}
	if count == 1 {
		return GlobalStatePosition{}, GlobalStatePosition{1, 0}, nil
	}
	batch, found, err := v.inboxTracker.FindInboxBatchContainingMessage(count - 1)
	if err != nil {
		return GlobalStatePosition{}, GlobalStatePosition{}, err
	}
	if !found {
		return GlobalStatePosition{}, GlobalStatePosition{}, errors.New("batch not found on L1 yet")
	}
	return GlobalStatePositionsAtCount(v.inboxTracker, count, batch)
}

func (v *StatelessBlockValidator) CreateReadyValidationEntry(ctx context.Context, pos arbutil.MessageIndex) (*validationEntry, error) {
	msg, err := v.streamer.GetMessage(pos)
	if err != nil {
		return nil, err
	}
	result, err := v.streamer.ResultAtMessageIndex(pos)
	if err != nil {
		return nil, err
	}
	var prevDelayed uint64
	prevResult := &execution.MessageResult{}
	if pos > 0 {
		prev, err := v.streamer.GetMessage(pos - 1)
		if err != nil {
			return nil, err
		}
		prevDelayed = prev.DelayedMessagesRead
		prevResult, err = v.streamer.ResultAtMessageIndex(pos - 1)
		if err != nil {
			return nil, err
		}
	}

	startPos, endPos, err := v.GlobalStatePositionsAtCount(pos + 1)
	if err != nil {
		return nil, fmt.Errorf("failed calculating position for validation: %w", err)
	}
	start := BuildGlobalState(*prevResult, startPos)
	end := BuildGlobalState(*result, endPos)
	found, fullBatchInfo, err := v.readFullBatch(ctx, start.Batch)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("batch %d not found", startPos.BatchNumber)
	}

	prevBatchNums, err := msg.Message.PastBatchesRequired()
	if err != nil {
		return nil, err
	}
	prevBatches := make([]validator.BatchInfo, 0, len(prevBatchNums))
	for _, batchNum := range prevBatchNums {
		data, err := v.readPostedBatch(ctx, batchNum)
		if err != nil {
			return nil, err
		}
		prevBatches = append(prevBatches, validator.BatchInfo{
			Number: batchNum,
			Data:   data,
		})
	}
	entry, err := newValidationEntry(pos, start, end, msg, fullBatchInfo, prevBatches, prevDelayed, v.streamer.ChainConfig())
	if err != nil {
		return nil, err
	}
	err = v.ValidationEntryRecord(ctx, entry)
	if err != nil {
		return nil, err
	}

	return entry, nil
}

func (v *StatelessBlockValidator) ValidateResult(
	ctx context.Context, pos arbutil.MessageIndex, useExec bool, moduleRoot common.Hash,
) (bool, *validator.GoGlobalState, error) {
	entry, err := v.CreateReadyValidationEntry(ctx, pos)
	if err != nil {
		return false, nil, err
	}
	var run validator.ValidationRun
	if !useExec {
		if v.redisValidator != nil {
			if validator.SpawnerSupportsModule(v.redisValidator, moduleRoot) {
				input, err := entry.ToInput(v.redisValidator.StylusArchs())
				if err != nil {
					return false, nil, err
				}
				run = v.redisValidator.Launch(input, moduleRoot)
			}
		}
	}
	if run == nil {
		for _, spawner := range v.execSpawners {
			if validator.SpawnerSupportsModule(spawner, moduleRoot) {
				input, err := entry.ToInput(spawner.StylusArchs())
				if err != nil {
					return false, nil, err
				}
				run = spawner.Launch(input, moduleRoot)
				break
			}
		}
	}
	if run == nil {
		return false, nil, fmt.Errorf("validation with WasmModuleRoot %v not supported by node", moduleRoot)
	}
	defer run.Cancel()
	gsEnd, err := run.Await(ctx)
	if err != nil || gsEnd != entry.End {
		return false, &gsEnd, err
	}
	return true, &entry.End, nil
}

func (v *StatelessBlockValidator) ValidationInputsAt(ctx context.Context, pos arbutil.MessageIndex, targets ...ethdb.WasmTarget) (server_api.InputJSON, error) {
	entry, err := v.CreateReadyValidationEntry(ctx, pos)
	if err != nil {
		return server_api.InputJSON{}, err
	}
	input, err := entry.ToInput(targets)
	if err != nil {
		return server_api.InputJSON{}, err
	}
	return *server_api.ValidationInputToJson(input), nil
}

func (v *StatelessBlockValidator) OverrideRecorder(t *testing.T, recorder execution.ExecutionRecorder) {
	v.recorder = recorder
}

func (v *StatelessBlockValidator) GetLatestWasmModuleRoot() common.Hash {
	return v.latestWasmModuleRoot
}

func (v *StatelessBlockValidator) Start(ctx_in context.Context) error {
	if v.redisValidator != nil {
		if err := v.redisValidator.Start(ctx_in); err != nil {
			return fmt.Errorf("starting execution spawner: %w", err)
		}
	}
	for _, spawner := range v.execSpawners {
		if err := spawner.Start(ctx_in); err != nil {
			return err
		}
	}
	return nil
}

func (v *StatelessBlockValidator) Stop() {
	for _, spawner := range v.execSpawners {
		spawner.Stop()
	}
	if v.redisValidator != nil {
		v.redisValidator.Stop()
	}
}
