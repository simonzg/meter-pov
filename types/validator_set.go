package types

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	//"github.com/ethereum/go-ethereum/crypto"
	//"github.com/ethereum/go-ethereum/rlp"
	//"github.com/vechain/thor/block"
	//"github.com/vechain/thor/chain"
	cmn "github.com/vechain/thor/libs/common"
	//"github.com/vechain/thor/runtime"
	//"github.com/vechain/thor/state"
	//"github.com/vechain/thor/tx"
	//"github.com/vechain/thor/xenv"
)

// ValidatorSet represent a set of *Validator at a given height.
// The validators can be fetched by address or index.
// The index is in order of .Address, so the indices are fixed
// for all rounds of a given blockchain height.
// On the other hand, the .AccumPower of each validator and
// the designated .GetProposer() of a set changes every round,
// upon calling .IncrementAccum().
// NOTE: Not goroutine-safe.
// NOTE: All get/set to validators should copy the value for safety.
type ValidatorSet struct {
	// NOTE: persisted via reflect, must be exported.
	Validators []*Validator `json:"validators"`
	Proposer   *Validator   `json:"proposer"`

	// cached (unexported)
	totalVotingPower int64
}

func NewValidatorSet(valz []*Validator) *ValidatorSet {
	if valz != nil && len(valz) == 0 {
		panic("validator set initialization slice cannot be an empty slice (but it can be nil)")
	}
	validators := make([]*Validator, len(valz))
	for i, val := range valz {
		validators[i] = val.Copy()
	}
	sort.Sort(ValidatorsByAddress(validators))
	vals := &ValidatorSet{
		Validators: validators,
	}
	if len(valz) > 0 {
		vals.IncrementAccum(1)
	}

	return vals
}

//validator slice itself is sorted.
func NewValidatorSet2(vals []*Validator) *ValidatorSet {
	validators := make([]*Validator, len(vals))
	for i, val := range vals {
		validators[i] = val.Copy()
	}
	vs := &ValidatorSet{
		Validators: validators,
	}

	return vs
}

// Nil or empty validator sets are invalid.
func (vals *ValidatorSet) IsNilOrEmpty() bool {
	return vals == nil || len(vals.Validators) == 0
}

// Increment Accum and update the proposer on a copy, and return it.
func (vals *ValidatorSet) CopyIncrementAccum(times int) *ValidatorSet {
	copy := vals.Copy()
	copy.IncrementAccum(times)
	return copy
}

// IncrementAccum increments accum of each validator and updates the
// proposer. Panics if validator set is empty.
func (vals *ValidatorSet) IncrementAccum(times int) {

	// Add VotingPower * times to each validator and order into heap.
	validatorsHeap := cmn.NewHeap()
	for _, val := range vals.Validators {
		// Check for overflow both multiplication and sum.
		val.Accum = safeAddClip(val.Accum, safeMulClip(val.VotingPower, int64(times)))
		validatorsHeap.PushComparable(val, accumComparable{val})
	}

	// Decrement the validator with most accum times times.
	for i := 0; i < times; i++ {
		mostest := validatorsHeap.Peek().(*Validator)
		// mind underflow
		mostest.Accum = safeSubClip(mostest.Accum, vals.TotalVotingPower())

		if i == times-1 {
			vals.Proposer = mostest
		} else {
			validatorsHeap.Update(mostest, accumComparable{mostest})
		}
	}
}

// Copy each validator into a new ValidatorSet
func (vals *ValidatorSet) Copy() *ValidatorSet {
	validators := make([]*Validator, len(vals.Validators))
	for i, val := range vals.Validators {
		// NOTE: must copy, since IncrementAccum updates in place.
		validators[i] = val.Copy()
	}
	return &ValidatorSet{
		Validators:       validators,
		Proposer:         vals.Proposer,
		totalVotingPower: vals.totalVotingPower,
	}
}

// HasAddress returns true if address given is in the validator set, false -
// otherwise.
func (vals *ValidatorSet) HasAddress(address []byte) bool {
	idx := sort.Search(len(vals.Validators), func(i int) bool {
		return bytes.Compare(address, vals.Validators[i].Address.Bytes()) <= 0
	})
	return idx < len(vals.Validators) && bytes.Equal(vals.Validators[idx].Address.Bytes(), address)
}

// GetByAddress returns an index of the validator with address and validator
// itself if found. Otherwise, -1 and nil are returned.
func (vals *ValidatorSet) GetByAddress(address []byte) (index int, val *Validator) {
	idx := sort.Search(len(vals.Validators), func(i int) bool {
		return bytes.Compare(address, vals.Validators[i].Address.Bytes()) <= 0
	})
	if idx < len(vals.Validators) && bytes.Equal(vals.Validators[idx].Address.Bytes(), address) {
		return idx, vals.Validators[idx].Copy()
	}
	return -1, nil
}

// GetByIndex returns the validator's address and validator itself by index.
// It returns nil values if index is less than 0 or greater or equal to
// len(ValidatorSet.Validators).
func (vals *ValidatorSet) GetByIndex(index int) (address []byte, val *Validator) {
	if index < 0 || index >= len(vals.Validators) {
		return nil, nil
	}
	val = vals.Validators[index]
	return val.Address.Bytes(), val.Copy()
}

// Size returns the length of the validator set.
func (vals *ValidatorSet) Size() int {
	return len(vals.Validators)
}

// TotalVotingPower returns the sum of the voting powers of all validators.
func (vals *ValidatorSet) TotalVotingPower() int64 {
	if vals.totalVotingPower == 0 {
		for _, val := range vals.Validators {
			// mind overflow
			vals.totalVotingPower = safeAddClip(vals.totalVotingPower, val.VotingPower)
		}
	}
	return vals.totalVotingPower
}

// GetProposer returns the current proposer. If the validator set is empty, nil
// is returned.
func (vals *ValidatorSet) GetProposer() (proposer *Validator) {
	if len(vals.Validators) == 0 {
		return nil
	}
	if vals.Proposer == nil {
		vals.Proposer = vals.findProposer()
	}
	return vals.Proposer.Copy()
}

func (vals *ValidatorSet) findProposer() *Validator {
	var proposer *Validator
	for _, val := range vals.Validators {
		if proposer == nil || !bytes.Equal(val.Address.Bytes(), proposer.Address.Bytes()) {
			proposer = proposer.CompareAccum(val)
		}
	}
	return proposer
}

/*********************
// Hash returns the Merkle root hash build using validators (as leaves) in the
// set.
func (vals *ValidatorSet) Hash() []byte {
	if len(vals.Validators) == 0 {
		return nil
	}
	hashers := make([]merkle.Hasher, len(vals.Validators))
	for i, val := range vals.Validators {
		hashers[i] = val
	}
	return merkle.SimpleHashFromHashers(hashers)
}
********************/

// Add adds val to the validator set and returns true. It returns false if val
// is already in the set.
func (vals *ValidatorSet) Add(val *Validator) (added bool) {
	val = val.Copy()
	idx := sort.Search(len(vals.Validators), func(i int) bool {
		return bytes.Compare(val.Address.Bytes(), vals.Validators[i].Address.Bytes()) <= 0
	})
	if idx >= len(vals.Validators) {
		vals.Validators = append(vals.Validators, val)
		// Invalidate cache
		vals.Proposer = nil
		vals.totalVotingPower = 0
		return true
	} else if bytes.Equal(vals.Validators[idx].Address.Bytes(), val.Address.Bytes()) {
		return false
	} else {
		newValidators := make([]*Validator, len(vals.Validators)+1)
		copy(newValidators[:idx], vals.Validators[:idx])
		newValidators[idx] = val
		copy(newValidators[idx+1:], vals.Validators[idx:])
		vals.Validators = newValidators
		// Invalidate cache
		vals.Proposer = nil
		vals.totalVotingPower = 0
		return true
	}
}

// Update updates val and returns true. It returns false if val is not present
// in the set.
func (vals *ValidatorSet) Update(val *Validator) (updated bool) {
	index, sameVal := vals.GetByAddress(val.Address.Bytes())
	if sameVal == nil {
		return false
	}
	vals.Validators[index] = val.Copy()
	// Invalidate cache
	vals.Proposer = nil
	vals.totalVotingPower = 0
	return true
}

// Remove deletes the validator with address. It returns the validator removed
// and true. If returns nil and false if validator is not present in the set.
func (vals *ValidatorSet) Remove(address []byte) (val *Validator, removed bool) {
	idx := sort.Search(len(vals.Validators), func(i int) bool {
		return bytes.Compare(address, vals.Validators[i].Address.Bytes()) <= 0
	})
	if idx >= len(vals.Validators) || !bytes.Equal(vals.Validators[idx].Address.Bytes(), address) {
		return nil, false
	}
	removedVal := vals.Validators[idx]
	newValidators := vals.Validators[:idx]
	if idx+1 < len(vals.Validators) {
		newValidators = append(newValidators, vals.Validators[idx+1:]...)
	}
	vals.Validators = newValidators
	// Invalidate cache
	vals.Proposer = nil
	vals.totalVotingPower = 0
	return removedVal, true
}

// Iterate will run the given function over the set.
func (vals *ValidatorSet) Iterate(fn func(index int, val *Validator) bool) {
	for i, val := range vals.Validators {
		stop := fn(i, val.Copy())
		if stop {
			break
		}
	}
}

func (vals *ValidatorSet) String() string {
	return vals.StringIndented("")
}

// String
func (vals *ValidatorSet) StringIndented(indent string) string {
	if vals == nil {
		return "nil-ValidatorSet"
	}
	valStrings := []string{}
	vals.Iterate(func(index int, val *Validator) bool {
		valStrings = append(valStrings, val.String())
		return false
	})
	return fmt.Sprintf(`ValidatorSet{
%s  Proposer: %v
%s  Validators:
%s    %v
%s}`,
		indent, vals.GetProposer().String(),
		indent,
		indent, strings.Join(valStrings, "\n"+indent+"    "),
		indent)

}

//-------------------------------------
// Implements sort for sorting validators by address.

// Sort validators by address
type ValidatorsByAddress []*Validator

func (valz ValidatorsByAddress) Len() int {
	return len(valz)
}

func (valz ValidatorsByAddress) Less(i, j int) bool {
	return bytes.Compare(valz[i].Address.Bytes(), valz[j].Address.Bytes()) == -1
}

func (valz ValidatorsByAddress) Swap(i, j int) {
	it := valz[i]
	valz[i] = valz[j]
	valz[j] = it
}

//-------------------------------------
// Use with Heap for sorting validators by accum

type accumComparable struct {
	*Validator
}

// We want to find the validator with the greatest accum.
func (ac accumComparable) Less(o interface{}) bool {
	other := o.(accumComparable).Validator
	larger := ac.CompareAccum(other)
	return bytes.Equal(larger.Address.Bytes(), ac.Address.Bytes())
}

/*******************
//----------------------------------------
// For testing

// RandValidatorSet returns a randomized validator set, useful for testing.
// NOTE: PrivValidator are in order.
// UNSTABLE
func RandValidatorSet(numValidators int, votingPower int64) (*ValidatorSet, []PrivValidator) {
	valz := make([]*Validator, numValidators)
	privValidators := make([]PrivValidator, numValidators)
	for i := 0; i < numValidators; i++ {
		val, privValidator := RandValidator(false, votingPower)
		valz[i] = val
		privValidators[i] = privValidator
	}
	vals := NewValidatorSet(valz)
	sort.Sort(PrivValidatorsByAddress(privValidators))
	return vals, privValidators
}
*******************/

///////////////////////////////////////////////////////////////////////////////
// Safe multiplication and addition/subtraction

func safeMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	if a == 1 {
		return b, false
	}
	if b == 1 {
		return a, false
	}
	if a == math.MinInt64 || b == math.MinInt64 {
		return -1, true
	}
	c := a * b
	return c, c/b != a
}

func safeAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return -1, true
	} else if b < 0 && a < math.MinInt64-b {
		return -1, true
	}
	return a + b, false
}

func safeSub(a, b int64) (int64, bool) {
	if b > 0 && a < math.MinInt64+b {
		return -1, true
	} else if b < 0 && a > math.MaxInt64+b {
		return -1, true
	}
	return a - b, false
}

func safeMulClip(a, b int64) int64 {
	c, overflow := safeMul(a, b)
	if overflow {
		if (a < 0 || b < 0) && !(a < 0 && b < 0) {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return c
}

func safeAddClip(a, b int64) int64 {
	c, overflow := safeAdd(a, b)
	if overflow {
		if b < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return c
}

func safeSubClip(a, b int64) int64 {
	c, overflow := safeSub(a, b)
	if overflow {
		if b > 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return c
}