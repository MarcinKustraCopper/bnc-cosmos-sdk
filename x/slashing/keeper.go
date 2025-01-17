package slashing

import (
	"fmt"
	"math"
	"time"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/cosmos/cosmos-sdk/bsc/rlp"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/pubsub"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/fees"
	"github.com/cosmos/cosmos-sdk/x/bank"
	"github.com/cosmos/cosmos-sdk/x/paramHub/types"
	param "github.com/cosmos/cosmos-sdk/x/params"
	"github.com/cosmos/cosmos-sdk/x/sidechain"
	sTypes "github.com/cosmos/cosmos-sdk/x/sidechain/types"
	stake "github.com/cosmos/cosmos-sdk/x/stake/types"
)

// Keeper of the slashing store
type Keeper struct {
	storeKey     sdk.StoreKey
	cdc          *codec.Codec
	validatorSet sdk.ValidatorSet
	paramspace   param.Subspace

	// codespace
	Codespace sdk.CodespaceType

	BankKeeper bank.Keeper
	ScKeeper   *sidechain.Keeper

	PbsbServer *pubsub.Server
}

// NewKeeper creates a slashing keeper
func NewKeeper(cdc *codec.Codec, key sdk.StoreKey, vs sdk.ValidatorSet, paramspace param.Subspace, codespace sdk.CodespaceType, bk bank.Keeper) Keeper {
	keeper := Keeper{
		storeKey:     key,
		cdc:          cdc,
		validatorSet: vs,
		paramspace:   paramspace.WithTypeTable(ParamTypeTable()),
		Codespace:    codespace,
		BankKeeper:   bk,
	}
	return keeper
}

func (k *Keeper) SetSideChain(scKeeper *sidechain.Keeper) {
	k.ScKeeper = scKeeper
	k.initIbc()
}

func (k *Keeper) initIbc() {
	err := k.ScKeeper.RegisterChannel(ChannelName, ChannelId, k)
	if err != nil {
		panic(fmt.Sprintf("register ibc channel failed, channel=%s, err=%s", ChannelName, err.Error()))
	}
}

func (k *Keeper) SetPbsbServer(server *pubsub.Server) {
	k.PbsbServer = server
}

// handle a validator signing two blocks at the same height
// power: power of the double-signing validator at the height of infraction
func (k Keeper) handleDoubleSign(ctx sdk.Context, addr crypto.Address, infractionHeight int64, timestamp time.Time, power int64) {
	logger := ctx.Logger().With("module", "x/slashing")
	time := ctx.BlockHeader().Time
	age := time.Sub(timestamp)
	consAddr := sdk.ConsAddress(addr)
	pubkey, err := k.getPubkey(ctx, addr)
	if err != nil {
		panic(fmt.Sprintf("Validator consensus-address %v not found", consAddr))
	}

	// Double sign too old
	maxEvidenceAge := k.MaxEvidenceAge(ctx)
	if age > maxEvidenceAge {
		logger.Info(fmt.Sprintf("Ignored double sign from %s at height %d, age of %d past max age of %d", pubkey.Address(), infractionHeight, age, maxEvidenceAge))
		return
	}

	// Double sign confirmed

	logger.Info(fmt.Sprintf("Confirmed double sign from %s at height %d, age of %d less than max age of %d", pubkey.Address(), infractionHeight, age, maxEvidenceAge))

	// We need to retrieve the stake distribution which signed the block, so we subtract ValidatorUpdateDelay from the evidence height.
	// Note that this *can* result in a negative "distributionHeight", up to -ValidatorUpdateDelay,
	// i.e. at the end of the pre-genesis block (none) = at the beginning of the genesis block.
	// That's fine since this is just used to filter unbonding delegations & redelegations.
	distributionHeight := infractionHeight - stake.ValidatorUpdateDelay

	// Cap the amount slashed to the penalty for the worst infraction
	// within the slashing period when this infraction was committed
	fraction := k.SlashFractionDoubleSign(ctx)
	revisedFraction := k.capBySlashingPeriod(ctx, consAddr, fraction, distributionHeight)
	logger.Info(fmt.Sprintf("Fraction slashed capped by slashing period from %v to %v", fraction, revisedFraction))

	// Slash validator
	// `power` is the int64 power of the validator as provided to/by
	// Tendermint. This value is validator.Tokens as sent to Tendermint via
	// ABCI, and now received as evidence.
	// The revisedFraction (which is the new fraction to be slashed) is passed
	// in separately to separately slash unbonding and rebonding delegations.
	k.validatorSet.Slash(ctx, consAddr, distributionHeight, power, revisedFraction)

	// Jail validator if not already jailed
	validator := k.validatorSet.ValidatorByConsAddr(ctx, consAddr)
	if !validator.GetJailed() {
		k.validatorSet.Jail(ctx, consAddr)
	}

	// Set or updated validator jail duration
	signInfo, found := k.getValidatorSigningInfo(ctx, consAddr)
	if !found {
		panic(fmt.Sprintf("Expected signing info for validator %s but not found", consAddr))
	}
	signInfo.JailedUntil = time.Add(k.DoubleSignUnbondDuration(ctx))
	k.setValidatorSigningInfo(ctx, consAddr, signInfo)
}

// handle a validator signature, must be called once per validator per block
// TODO refactor to take in a consensus address, additionally should maybe just take in the pubkey too
func (k Keeper) handleValidatorSignature(ctx sdk.Context, addr crypto.Address, power int64, signed bool) {
	logger := ctx.Logger().With("module", "x/slashing")
	height := ctx.BlockHeight()
	consAddr := sdk.ConsAddress(addr)
	pubkey, err := k.getPubkey(ctx, addr)
	if err != nil {
		panic(fmt.Sprintf("Validator consensus-address %v not found", consAddr))
	}
	// Local index, so counts blocks validator *should* have signed
	// Will use the 0-value default signing info if not present, except for start height
	signInfo, found := k.getValidatorSigningInfo(ctx, consAddr)
	if !found {
		panic(fmt.Sprintf("Expected signing info for validator %s but not found", consAddr))
	}
	index := signInfo.IndexOffset % k.SignedBlocksWindow(ctx)
	signInfo.IndexOffset++

	// Update signed block bit array & counter
	// This counter just tracks the sum of the bit array
	// That way we avoid needing to read/write the whole array each time
	previous := k.getValidatorMissedBlockBitArray(ctx, consAddr, index)
	missed := !signed
	switch {
	case !previous && missed:
		// Array value has changed from not missed to missed, increment counter
		k.setValidatorMissedBlockBitArray(ctx, consAddr, index, true)
		signInfo.MissedBlocksCounter++
	case previous && !missed:
		// Array value has changed from missed to not missed, decrement counter
		k.setValidatorMissedBlockBitArray(ctx, consAddr, index, false)
		signInfo.MissedBlocksCounter--
	default:
		// Array value at this index has not changed, no need to update counter
	}

	if missed {
		logger.Info(fmt.Sprintf("Absent validator %s at height %d, %d missed, threshold %d", addr, height, signInfo.MissedBlocksCounter, k.MinSignedPerWindow(ctx)))
	}
	minHeight := signInfo.StartHeight + k.SignedBlocksWindow(ctx)
	maxMissed := k.SignedBlocksWindow(ctx) - k.MinSignedPerWindow(ctx)
	if height > minHeight && signInfo.MissedBlocksCounter > maxMissed {
		validator := k.validatorSet.ValidatorByConsAddr(ctx, consAddr)
		if validator != nil && !validator.GetJailed() {
			// Downtime confirmed: slash and jail the validator
			logger.Info(fmt.Sprintf("Validator %s past min height of %d and below signed blocks threshold of %d",
				pubkey.Address(), minHeight, k.MinSignedPerWindow(ctx)))
			// We need to retrieve the stake distribution which signed the block, so we subtract ValidatorUpdateDelay from the evidence height,
			// and subtract an additional 1 since this is the LastCommit.
			// Note that this *can* result in a negative "distributionHeight" up to -ValidatorUpdateDelay-1,
			// i.e. at the end of the pre-genesis block (none) = at the beginning of the genesis block.
			// That's fine since this is just used to filter unbonding delegations & redelegations.
			distributionHeight := height - stake.ValidatorUpdateDelay - 1
			k.validatorSet.Slash(ctx, consAddr, distributionHeight, power, k.SlashFractionDowntime(ctx))
			k.validatorSet.Jail(ctx, consAddr)
			signInfo.JailedUntil = ctx.BlockHeader().Time.Add(k.DowntimeUnbondDuration(ctx))
			// We need to reset the counter & array so that the validator won't be immediately slashed for downtime upon rebonding.
			signInfo.MissedBlocksCounter = 0
			signInfo.IndexOffset = 0
			k.clearValidatorMissedBlockBitArray(ctx, consAddr)
		} else {
			// Validator was (a) not found or (b) already jailed, don't slash
			logger.Info(fmt.Sprintf("Validator %s would have been slashed for downtime, but was either not found in store or already jailed",
				pubkey.Address()))
		}
	}

	// Set the updated signing info
	k.setValidatorSigningInfo(ctx, consAddr, signInfo)
}

// AddValidators adds the validators to the keepers validator addr to pubkey mapping.
func (k Keeper) AddValidators(ctx sdk.Context, vals []abci.ValidatorUpdate) {
	for i := 0; i < len(vals); i++ {
		val := vals[i]
		pubkey, err := tmtypes.PB2TM.PubKey(val.PubKey)
		if err != nil {
			panic(err)
		}
		k.addPubkey(ctx, pubkey)
	}
}

func (k *Keeper) SubscribeParamChange(hub types.ParamChangePublisher) {
	hub.SubscribeParamChange(
		func(context sdk.Context, iChange interface{}) {
			switch change := iChange.(type) {
			case *Params:
				// do double check
				err := change.UpdateCheck()
				if err != nil {
					context.Logger().Error("skip invalid param change", "err", err, "param", change)
				} else {
					k.SetParams(context, *change)
					break
				}
			default:
				context.Logger().Debug("skip unknown param change")
			}
		},
		&types.ParamSpaceProto{ParamSpace: k.paramspace, Proto: func() types.SCParam {
			return new(Params)
		}},
		nil,
		nil,
	)
}

// implement cross chain app
func (k *Keeper) ExecuteSynPackage(ctx sdk.Context, payload []byte, _ int64) sdk.ExecuteResult {
	var resCode uint32
	sideSlashPack, err := k.checkSideSlashPackage(payload)
	if err == nil {
		if sideSlashPack.addrType == SideConsAddrType {
			err = k.slashingSideDowntime(ctx, sideSlashPack)
		} else if sideSlashPack.addrType == SideVoteAddrType {
			err = k.slashingSideMaliciousVote(ctx, sideSlashPack)
		}
	}
	if err != nil {
		resCode = uint32(err.ABCICode())
	}
	ackPackage, encodeErr := sTypes.GenCommonAckPackage(resCode)
	if encodeErr != nil {
		panic(encodeErr)
	}
	return sdk.ExecuteResult{
		Payload: ackPackage,
		Err:     err,
		Tags:    sdk.EmptyTags(),
	}
}
func (k *Keeper) ExecuteAckPackage(ctx sdk.Context, payload []byte) sdk.ExecuteResult {
	panic("receive unexpected ack package")
}

// When the ack application crash, payload is the payload of the origin package.
func (k *Keeper) ExecuteFailAckPackage(ctx sdk.Context, payload []byte) sdk.ExecuteResult {
	panic("receive unexpected fail ack package")
}

func (k *Keeper) checkSideSlashPackage(payload []byte) (*SideSlashPackage, sdk.Error) {
	var slashEvent SideSlashPackage
	err := rlp.DecodeBytes(payload, &slashEvent)
	if err != nil {
		return nil, ErrInvalidInput(k.Codespace, "failed to parse the payload")
	}

	if len(slashEvent.SideAddr) == sdk.AddrLen {
		slashEvent.addrType = SideConsAddrType
	} else if len(slashEvent.SideAddr) == sdk.VoteAddrLen {
		slashEvent.addrType = SideVoteAddrType
	} else {
		return nil, ErrInvalidClaim(k.Codespace, fmt.Sprintf("wrong sideAddr length:%d, expected:%d or %d", len(slashEvent.SideAddr), sdk.AddrLen, sdk.VoteAddrLen))
	}

	if slashEvent.SideHeight <= 0 {
		return nil, ErrInvalidClaim(k.Codespace, "side height must be positive")
	}

	if slashEvent.SideHeight > math.MaxInt64 {
		return nil, ErrInvalidClaim(k.Codespace, "side height overflow")
	}

	if slashEvent.SideTimestamp <= 0 {
		return nil, ErrInvalidClaim(k.Codespace, "invalid side timestamp")
	}
	return &slashEvent, nil
}

func (k *Keeper) slashingSideDowntime(ctx sdk.Context, pack *SideSlashPackage) sdk.Error {
	sideConsAddr := pack.SideAddr
	sideChainName, err := k.ScKeeper.GetDestChainName(pack.SideChainId)
	if err != nil {
		return ErrInvalidSideChainId(DefaultCodespace)
	}
	sideCtx, err := k.ScKeeper.PrepareCtxForSideChain(ctx, sideChainName)
	if err != nil {
		return ErrInvalidSideChainId(DefaultCodespace)
	}

	header := sideCtx.BlockHeader()
	age := uint64(header.Time.Unix()) - pack.SideTimestamp
	if age > uint64(k.MaxEvidenceAge(sideCtx).Seconds()) {
		return ErrExpiredEvidence(DefaultCodespace)
	}

	if k.hasSlashRecord(sideCtx, sideConsAddr, Downtime, pack.SideHeight) {
		return ErrDuplicateDowntimeClaim(k.Codespace)
	}

	slashAmt := k.DowntimeSlashAmount(sideCtx)
	validator, slashedAmt, err := k.validatorSet.SlashSideChain(ctx, sideChainName, sideConsAddr, sdk.NewDec(slashAmt))
	if err != nil {
		return ErrFailedToSlash(k.Codespace, err.Error())
	}

	downtimeClaimFee := k.DowntimeSlashFee(sideCtx)
	downtimeClaimFeeReal := sdk.MinInt64(downtimeClaimFee, slashedAmt.RawInt())
	var toFeePool int64
	bondDenom := k.validatorSet.BondDenom(sideCtx)
	if downtimeClaimFeeReal > 0 && ctx.IsDeliverTx() {
		feeCoinAdd := sdk.NewCoin(bondDenom, downtimeClaimFeeReal)
		fees.Pool.AddAndCommitFee("side_downtime_slash", sdk.NewFee(sdk.Coins{feeCoinAdd}, sdk.FeeForAll))
		toFeePool = downtimeClaimFeeReal
	}

	remaining := slashedAmt.RawInt() - downtimeClaimFeeReal
	var validatorsAllocatedAmt map[string]int64
	var found bool
	if remaining > 0 {
		found, validatorsAllocatedAmt, err = k.validatorSet.AllocateSlashAmtToValidators(sideCtx, sideConsAddr, sdk.NewDec(remaining))
		if err != nil {
			return ErrFailedToSlash(k.Codespace, err.Error())
		}
		if !found && ctx.IsDeliverTx() {
			remainingCoin := sdk.NewCoin(bondDenom, remaining)
			fees.Pool.AddAndCommitFee("side_downtime_slash_remaining", sdk.NewFee(sdk.Coins{remainingCoin}, sdk.FeeForAll))
			toFeePool = toFeePool + remaining
		}
	}

	jailUntil := header.Time.Add(k.DowntimeUnbondDuration(sideCtx))
	sr := SlashRecord{
		ConsAddr:         sideConsAddr,
		InfractionType:   Downtime,
		InfractionHeight: pack.SideHeight,
		SlashHeight:      header.Height,
		JailUntil:        jailUntil,
		SlashAmt:         slashedAmt.RawInt(),
		SideChainId:      sideChainName,
	}
	k.setSlashRecord(sideCtx, sr)

	// Set or updated validator jail duration
	signInfo, found := k.getValidatorSigningInfo(sideCtx, sideConsAddr)
	if !found {
		return sdk.ErrInternal(fmt.Sprintf("Expected signing info for validator %s but not found", sdk.HexEncode(sideConsAddr)))
	}
	//if jailUntil.After(signInfo.JailedUntil) {
	signInfo.JailedUntil = jailUntil
	//}
	k.setValidatorSigningInfo(sideCtx, sideConsAddr, signInfo)

	if k.PbsbServer != nil {
		event := SideSlashEvent{
			Validator:              validator.GetOperator(),
			InfractionType:         Downtime,
			InfractionHeight:       int64(pack.SideHeight),
			SlashHeight:            header.Height,
			JailUtil:               jailUntil,
			SlashAmt:               slashedAmt.RawInt(),
			ToFeePool:              toFeePool,
			SideChainId:            sideChainName,
			ValidatorsCompensation: validatorsAllocatedAmt,
		}
		k.PbsbServer.Publish(event)
	}

	return nil
}

func (k *Keeper) slashingSideMaliciousVote(ctx sdk.Context, pack *SideSlashPackage) sdk.Error {
	logger := ctx.Logger().With("module", "x/slashing")
	sideVoteAddr := pack.SideAddr
	sideChainName, err := k.ScKeeper.GetDestChainName(pack.SideChainId)
	if err != nil {
		return ErrInvalidSideChainId(DefaultCodespace)
	}
	sideCtx, err := k.ScKeeper.PrepareCtxForSideChain(ctx, sideChainName)
	if err != nil {
		return ErrInvalidSideChainId(DefaultCodespace)
	}

	header := sideCtx.BlockHeader()
	age := uint64(header.Time.Unix()) - pack.SideTimestamp
	maxEvidenceAge := uint64(k.MaxEvidenceAge(sideCtx).Seconds())
	if age > maxEvidenceAge {
		return ErrExpiredEvidence(DefaultCodespace)
	}

	validator := k.validatorSet.ValidatorByVoteAddr(sideCtx, sideVoteAddr)
	if validator == nil {
		return ErrNoValidatorWithVoteAddr(k.Codespace)
	}

	sideConsAddr := []byte(validator.GetSideChainConsAddr())
	signInfo, found := k.getValidatorSigningInfo(sideCtx, sideConsAddr)
	if !found {
		return sdk.ErrInternal(fmt.Sprintf("Expected signing info for validator %s but not found", sdk.HexEncode(sideConsAddr)))
	}
	// in duration of malicious vote slash, validator can only be slashed once, to protect validator from funds drained
	if k.isMaliciousVoteSlashed(sideCtx, sideConsAddr) && pack.SideTimestamp < uint64(signInfo.JailedUntil.Unix()) {
		logger.Info(fmt.Sprintf("slashing is blocked because %s is still in duration of lastest malicious vote slash", sideConsAddr))
		return ErrFailedToSlash(k.Codespace, "still in duration of lastest malicious vote slash")
	} else if k.hasSlashRecord(sideCtx, sideConsAddr, MaliciousVote, pack.SideHeight) {
		logger.Info("slashing is blocked for duplicate malicious vote claim")
		return ErrDuplicateMaliciousVoteClaim(k.Codespace)
	}

	// Malicious vote confirmed
	logger.Info(fmt.Sprintf("Confirmed malicious vote from %s at height %d, age %d is less than max age %d, summit at %d, jailed until %d before slashing",
		sdk.HexAddress(sideConsAddr), pack.SideHeight, age, maxEvidenceAge, pack.SideTimestamp, uint64(signInfo.JailedUntil.Unix())))

	slashAmt := k.DoubleSignSlashAmount(sideCtx)
	validator, slashedAmt, err := k.validatorSet.SlashSideChain(ctx, sideChainName, sideConsAddr, sdk.NewDec(slashAmt))
	if err != nil {
		return ErrFailedToSlash(k.Codespace, err.Error())
	}

	var toFeePool int64
	var validatorsCompensation map[string]int64
	if slashAmt > 0 {
		found, validatorsCompensation, err = k.validatorSet.AllocateSlashAmtToValidators(sideCtx, sideConsAddr, sdk.NewDec(slashAmt))
		if err != nil {
			return ErrFailedToSlash(k.Codespace, err.Error())
		}
		if !found && ctx.IsDeliverTx() {
			bondDenom := k.validatorSet.BondDenom(sideCtx)
			toFeePool = slashAmt
			remainingCoin := sdk.NewCoin(bondDenom, slashAmt)
			fees.Pool.AddAndCommitFee("side_malicious_vote_slash", sdk.NewFee(sdk.Coins{remainingCoin}, sdk.FeeForAll))
		}
	}

	// Set or updated validator jail duration
	jailUntil := header.Time.Add(k.DoubleSignUnbondDuration(sideCtx))
	sr := SlashRecord{
		ConsAddr:         sideConsAddr,
		InfractionType:   MaliciousVote,
		InfractionHeight: pack.SideHeight,
		SlashHeight:      header.Height,
		JailUntil:        jailUntil,
		SlashAmt:         slashedAmt.RawInt(),
		SideChainId:      sideChainName,
	}
	k.setSlashRecord(sideCtx, sr)

	if jailUntil.After(signInfo.JailedUntil) {
		signInfo.JailedUntil = jailUntil
	}
	k.setValidatorSigningInfo(sideCtx, sideConsAddr, signInfo)

	if k.PbsbServer != nil {
		event := SideSlashEvent{
			Validator:              validator.GetOperator(),
			InfractionType:         MaliciousVote,
			InfractionHeight:       int64(pack.SideHeight),
			SlashHeight:            header.Height,
			JailUtil:               jailUntil,
			SlashAmt:               slashedAmt.RawInt(),
			ToFeePool:              toFeePool,
			SideChainId:            sideChainName,
			ValidatorsCompensation: validatorsCompensation,
		}
		k.PbsbServer.Publish(event)
	}

	return nil
}

// TODO: Make a method to remove the pubkey from the map when a validator is unbonded.
func (k Keeper) addPubkey(ctx sdk.Context, pubkey crypto.PubKey) {
	addr := pubkey.Address()
	k.setAddrPubkeyRelation(ctx, addr, pubkey)
}

func (k Keeper) getPubkey(ctx sdk.Context, address crypto.Address) (crypto.PubKey, error) {
	store := ctx.KVStore(k.storeKey)
	var pubkey crypto.PubKey
	err := k.cdc.UnmarshalBinaryLengthPrefixed(store.Get(getAddrPubkeyRelationKey(address)), &pubkey)
	if err != nil {
		return nil, fmt.Errorf("address %v not found", address)
	}
	return pubkey, nil
}

func (k Keeper) setAddrPubkeyRelation(ctx sdk.Context, addr crypto.Address, pubkey crypto.PubKey) {
	store := ctx.KVStore(k.storeKey)
	bz := k.cdc.MustMarshalBinaryLengthPrefixed(pubkey)
	store.Set(getAddrPubkeyRelationKey(addr), bz)
}

func (k Keeper) deleteAddrPubkeyRelation(ctx sdk.Context, addr crypto.Address) {
	store := ctx.KVStore(k.storeKey)
	store.Delete(getAddrPubkeyRelationKey(addr))
}
