package keeper

import (
	"encoding/binary"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/stake/types"
)

// TODO remove some of these prefixes once have working multistore

// nolint
var (
	// Keys for store prefixes
	// TODO DEPRECATED: delete in next release and reorder keys
	// ParamKey                         = []byte{0x00} // key for parameters relating to staking
	PoolKey                         = []byte{0x01} // key for the staking pools
	IntraTxCounterKey               = []byte{0x02} // key for intra-block tx index
	WhiteLabelOracleRelayerKey      = []byte{0x03} // key for white label oracle relayer
	PendingValidatorUpdateKey       = []byte{0x04} // key for pending validator update
	PrevProposerDistributionAddrKey = []byte{0x05} // key for previous proposer distribution address

	// Last* values are const during a block.
	LastValidatorPowerKey = []byte{0x11} // prefix for each key to a validator index, for bonded validators
	LastTotalPowerKey     = []byte{0x12} // prefix for the total power

	ValidatorsKey               = []byte{0x21} // prefix for each key to a validator
	ValidatorsByConsAddrKey     = []byte{0x22} // prefix for each key to a validator index, by pubkey
	ValidatorsByPowerIndexKey   = []byte{0x23} // prefix for each key to a validator index, sorted by power
	ValidatorsByHeightKey       = []byte{0x24} // prefix for each key to a validator index, by height
	ValidatorsBySideVoteAddrKey = []byte{0x25} // prefix for each key to a validator index, by vote address

	DelegationKey                    = []byte{0x31} // key for a delegation
	UnbondingDelegationKey           = []byte{0x32} // key for an unbonding-delegation
	UnbondingDelegationByValIndexKey = []byte{0x33} // prefix for each key for an unbonding-delegation, by validator operator
	RedelegationKey                  = []byte{0x34} // key for a redelegation
	RedelegationByValSrcIndexKey     = []byte{0x35} // prefix for each key for an redelegation, by source validator operator
	RedelegationByValDstIndexKey     = []byte{0x36} // prefix for each key for an redelegation, by destination validator operator
	DelegationKeyByVal               = []byte{0x37} // prefix for each key for a delegation, by validator operator and delegator
	SimplifiedDelegationsKey         = []byte{0x38} // prefix for each key for an simplifiedDelegations, by height and validator operator
	ValLatestUpdateConsAddrTimeKey   = []byte{0x39} // prefix for each key for an latest update ConsAddr time, by validator operator

	UnbondingQueueKey    = []byte{0x41} // prefix for the timestamps in unbonding queue
	RedelegationQueueKey = []byte{0x42} // prefix for the timestamps in redelegations queue
	ValidatorQueueKey    = []byte{0x43} // prefix for the timestamps in validator queue

	SideChainStorePrefixByIdKey = []byte{0x51} // prefix for each key to a side chain store prefix, by side chain id

	// Keys for reward store prefix
	RewardBatchKey       = []byte{0x01} // key for batch of rewards
	RewardValDistAddrKey = []byte{0x02} // key for rewards' validator <-> distribution address mapping
)

const (
	maxDigitsForAccount = 12 // ~220,000,000 atoms created at launch
)

// gets the key for the validator with address
// VALUE: stake/types.Validator
func GetValidatorKey(operatorAddr sdk.ValAddress) []byte {
	return append(ValidatorsKey, operatorAddr.Bytes()...)
}

// gets the key for the validator with pubkey
// VALUE: validator operator address ([]byte)
func GetValidatorByConsAddrKey(addr sdk.ConsAddress) []byte {
	return append(ValidatorsByConsAddrKey, addr.Bytes()...)
}

// gets the key for the validator with sideConsAddr
// VALUE: validator operator address ([]byte)
// NOTE: here we reuse the `ValidatorsByConsAddrKey` as side chain validator does not need a main chain pubkey(consAddr).
func GetValidatorBySideConsAddrKey(sideConsAddr []byte) []byte {
	return append(ValidatorsByConsAddrKey, sideConsAddr...)
}

func GetValidatorBySideVoteAddrKey(sideVoteAddr []byte) []byte {
	return append(ValidatorsBySideVoteAddrKey, sideVoteAddr...)
}

// Get the validator operator address from LastValidatorPowerKey
func AddressFromLastValidatorPowerKey(key []byte) []byte {
	return key[1:] // remove prefix bytes
}

// get the validator by power index.
// Power index is the key used in the power-store, and represents the relative
// power ranking of the validator.
// VALUE: validator operator address ([]byte)
func GetValidatorsByPowerIndexKey(validator types.Validator) []byte {
	var keyBytes []byte
	sdk.Upgrade(sdk.LaunchBscUpgrade, func() {
		keyBytes = getValidatorPowerRank(validator)
	}, nil, func() {
		keyBytes = getValidatorPowerRankNew(validator)
	})
	return keyBytes
}

// get the bonded validator index key for an operator address
func GetLastValidatorPowerKey(operator sdk.ValAddress) []byte {
	return append(LastValidatorPowerKey, operator...)
}

// get the power ranking of a validator
// NOTE the larger values are of higher value
// nolint: unparam
func getValidatorPowerRank(validator types.Validator) []byte {
	tendermintPower := validator.Tokens.RawInt()
	tendermintPowerBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(tendermintPowerBytes[:], uint64(tendermintPower))

	powerBytes := tendermintPowerBytes
	powerBytesLen := len(powerBytes)

	// key is of format prefix || powerbytes || heightBytes || counterBytes
	key := make([]byte, 1+powerBytesLen+8+2)

	key[0] = ValidatorsByPowerIndexKey[0]
	copy(key[1:powerBytesLen+1], powerBytes)

	// include heightBytes height is inverted (older validators first)
	binary.BigEndian.PutUint64(key[powerBytesLen+1:powerBytesLen+9], ^uint64(validator.BondHeight))
	// include counterBytes, counter is inverted (first txns have priority)
	binary.BigEndian.PutUint16(key[powerBytesLen+9:powerBytesLen+11], ^uint16(validator.BondIntraTxCounter))
	return key
}

func getValidatorPowerRankNew(validator types.Validator) []byte {
	power := validator.Tokens.RawInt()

	prefixLen := len(ValidatorsByPowerIndexKey)
	powerLen := 8
	// key is of format prefix || powerbytes || addrBytes
	key := make([]byte, prefixLen+powerLen+sdk.AddrLen)
	copy(key[:prefixLen], ValidatorsByPowerIndexKey)
	binary.BigEndian.PutUint64(key[prefixLen:prefixLen+powerLen], uint64(power))

	operAddrInvr := make([]byte, len(validator.OperatorAddr))
	copy(operAddrInvr, validator.OperatorAddr)
	for i, b := range operAddrInvr {
		operAddrInvr[i] = ^b
	}
	copy(key[prefixLen+powerLen:], operAddrInvr)
	return key
}

func GetValidatorHeightKey(height int64) []byte {
	bz := make([]byte, 8)
	binary.BigEndian.PutUint64(bz, uint64(height))
	return append(ValidatorsByHeightKey, bz...)
}

// gets the prefix for all unbonding delegations from a delegator
func GetValidatorQueueTimeKey(timestamp time.Time) []byte {
	bz := sdk.FormatTimeBytes(timestamp)
	return append(ValidatorQueueKey, bz...)
}

//______________________________________________________________________________

// gets the key for delegator bond with validator
// VALUE: stake/types.Delegation
func GetDelegationKey(delAddr sdk.AccAddress, valAddr sdk.ValAddress) []byte {
	return append(GetDelegationsKey(delAddr), valAddr.Bytes()...)
}

// gets the prefix for a delegator for all validators
func GetDelegationsKey(delAddr sdk.AccAddress) []byte {
	return append(DelegationKey, delAddr.Bytes()...)
}

//______________________________________________________________________________

// gets the key for validator bond with delegator
func GetDelegationKeyByValIndexKey(valAddr sdk.ValAddress, delAddr sdk.AccAddress) []byte {
	return append(GetDelegationsKeyByVal(valAddr), delAddr.Bytes()...)
}

// gets the prefix for a validator for all delegator
func GetDelegationsKeyByVal(valAddr sdk.ValAddress) []byte {
	return append(DelegationKeyByVal, valAddr.Bytes()...)
}

//______________________________________________________________________________

// gets the prefix for an array of simplified delegation for particular validator and height
// VALUE: []stake/types.SimplifiedDelegation
func GetSimplifiedDelegationsKey(height int64, valAddr sdk.ValAddress) []byte {
	heightBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(heightBytes, uint64(height))
	return append(append(SimplifiedDelegationsKey, heightBytes...), valAddr.Bytes()...)
}

//______________________________________________________________________________

// gets the key for an unbonding delegation by delegator and validator addr
// VALUE: stake/types.UnbondingDelegation
func GetUBDKey(delAddr sdk.AccAddress, valAddr sdk.ValAddress) []byte {
	return append(
		GetUBDsKey(delAddr.Bytes()),
		valAddr.Bytes()...)
}

// gets the index-key for an unbonding delegation, stored by validator-index
// VALUE: none (key rearrangement used)
func GetUBDByValIndexKey(delAddr sdk.AccAddress, valAddr sdk.ValAddress) []byte {
	return append(GetUBDsByValIndexKey(valAddr), delAddr.Bytes()...)
}

// rearranges the ValIndexKey to get the UBDKey
func GetUBDKeyFromValIndexKey(IndexKey []byte) []byte {
	addrs := IndexKey[1:] // remove prefix bytes
	if len(addrs) != 2*sdk.AddrLen {
		panic("unexpected key length")
	}
	valAddr := addrs[:sdk.AddrLen]
	delAddr := addrs[sdk.AddrLen:]
	return GetUBDKey(delAddr, valAddr)
}

//______________

// gets the prefix for all unbonding delegations from a delegator
func GetUBDsKey(delAddr sdk.AccAddress) []byte {
	return append(UnbondingDelegationKey, delAddr.Bytes()...)
}

// gets the prefix keyspace for the indexes of unbonding delegations for a validator
func GetUBDsByValIndexKey(valAddr sdk.ValAddress) []byte {
	return append(UnbondingDelegationByValIndexKey, valAddr.Bytes()...)
}

// gets the prefix for all unbonding delegations from a delegator
func GetUnbondingDelegationTimeKey(timestamp time.Time) []byte {
	bz := sdk.FormatTimeBytes(timestamp)
	return append(UnbondingQueueKey, bz...)
}

//________________________________________________________________________________

// gets the key for a redelegation
// VALUE: stake/types.RedelegationKey
func GetREDKey(delAddr sdk.AccAddress, valSrcAddr, valDstAddr sdk.ValAddress) []byte {
	key := make([]byte, 1+sdk.AddrLen*3)

	copy(key[0:sdk.AddrLen+1], GetREDsKey(delAddr.Bytes()))
	copy(key[sdk.AddrLen+1:2*sdk.AddrLen+1], valSrcAddr.Bytes())
	copy(key[2*sdk.AddrLen+1:3*sdk.AddrLen+1], valDstAddr.Bytes())

	return key
}

// gets the index-key for a redelegation, stored by source-validator-index
// VALUE: none (key rearrangement used)
func GetREDByValSrcIndexKey(delAddr sdk.AccAddress, valSrcAddr, valDstAddr sdk.ValAddress) []byte {
	REDSFromValsSrcKey := GetREDsFromValSrcIndexKey(valSrcAddr)
	offset := len(REDSFromValsSrcKey)

	// key is of the form REDSFromValsSrcKey || delAddr || valDstAddr
	key := make([]byte, len(REDSFromValsSrcKey)+2*sdk.AddrLen)
	copy(key[0:offset], REDSFromValsSrcKey)
	copy(key[offset:offset+sdk.AddrLen], delAddr.Bytes())
	copy(key[offset+sdk.AddrLen:offset+2*sdk.AddrLen], valDstAddr.Bytes())
	return key
}

// gets the index-key for a redelegation, stored by destination-validator-index
// VALUE: none (key rearrangement used)
func GetREDByValDstIndexKey(delAddr sdk.AccAddress, valSrcAddr, valDstAddr sdk.ValAddress) []byte {
	REDSToValsDstKey := GetREDsToValDstIndexKey(valDstAddr)
	offset := len(REDSToValsDstKey)

	// key is of the form REDSToValsDstKey || delAddr || valSrcAddr
	key := make([]byte, len(REDSToValsDstKey)+2*sdk.AddrLen)
	copy(key[0:offset], REDSToValsDstKey)
	copy(key[offset:offset+sdk.AddrLen], delAddr.Bytes())
	copy(key[offset+sdk.AddrLen:offset+2*sdk.AddrLen], valSrcAddr.Bytes())

	return key
}

// GetREDKeyFromValSrcIndexKey rearranges the ValSrcIndexKey to get the REDKey
func GetREDKeyFromValSrcIndexKey(indexKey []byte) []byte {
	// note that first byte is prefix byte
	if len(indexKey) != 3*sdk.AddrLen+1 {
		panic("unexpected key length")
	}
	valSrcAddr := indexKey[1 : sdk.AddrLen+1]
	delAddr := indexKey[sdk.AddrLen+1 : 2*sdk.AddrLen+1]
	valDstAddr := indexKey[2*sdk.AddrLen+1 : 3*sdk.AddrLen+1]

	return GetREDKey(delAddr, valSrcAddr, valDstAddr)
}

// GetREDKeyFromValDstIndexKey rearranges the ValDstIndexKey to get the REDKey
func GetREDKeyFromValDstIndexKey(indexKey []byte) []byte {
	// note that first byte is prefix byte
	if len(indexKey) != 3*sdk.AddrLen+1 {
		panic("unexpected key length")
	}
	valDstAddr := indexKey[1 : sdk.AddrLen+1]
	delAddr := indexKey[sdk.AddrLen+1 : 2*sdk.AddrLen+1]
	valSrcAddr := indexKey[2*sdk.AddrLen+1 : 3*sdk.AddrLen+1]
	return GetREDKey(delAddr, valSrcAddr, valDstAddr)
}

// gets the prefix for all unbonding delegations from a delegator
func GetRedelegationTimeKey(timestamp time.Time) []byte {
	bz := sdk.FormatTimeBytes(timestamp)
	return append(RedelegationQueueKey, bz...)
}

//______________

// gets the prefix keyspace for redelegations from a delegator
func GetREDsKey(delAddr sdk.AccAddress) []byte {
	return append(RedelegationKey, delAddr.Bytes()...)
}

// gets the prefix keyspace for all redelegations redelegating away from a source validator
func GetREDsFromValSrcIndexKey(valSrcAddr sdk.ValAddress) []byte {
	return append(RedelegationByValSrcIndexKey, valSrcAddr.Bytes()...)
}

// gets the prefix keyspace for all redelegations redelegating towards a destination validator
func GetREDsToValDstIndexKey(valDstAddr sdk.ValAddress) []byte {
	return append(RedelegationByValDstIndexKey, valDstAddr.Bytes()...)
}

// gets the prefix keyspace for all redelegations redelegating towards a destination validator
// from a particular delegator
func GetREDsByDelToValDstIndexKey(delAddr sdk.AccAddress, valDstAddr sdk.ValAddress) []byte {
	return append(
		GetREDsToValDstIndexKey(valDstAddr),
		delAddr.Bytes()...)
}

func GetValLatestUpdateConsAddrTimeKey(valAddr sdk.ValAddress) []byte {
	return append(ValLatestUpdateConsAddrTimeKey, valAddr.Bytes()...)
}
