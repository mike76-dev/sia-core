// Package consensus implements the Sia consensus algorithms.
package consensus

import (
	"encoding/binary"
	"math/big"
	"math/bits"
	"sort"
	"sync"
	"time"

	"go.sia.tech/core/internal/blake2b"
	"go.sia.tech/core/types"
)

// Pool for reducing heap allocations when hashing. This is only necessary
// because blake2b.New256 returns a hash.Hash interface, which prevents the
// compiler from doing escape analysis. Can be removed if we switch to an
// implementation whose constructor returns a concrete type.
var hasherPool = &sync.Pool{New: func() interface{} { return types.NewHasher() }}

// A Network specifies the fixed parameters of a Sia blockchain.
type Network struct {
	Name string `json:"name"`

	InitialCoinbase types.Currency `json:"initialCoinbase"`
	MinimumCoinbase types.Currency `json:"minimumCoinbase"`
	InitialTarget   types.BlockID  `json:"initialTarget"`

	HardforkDevAddr struct {
		Height     uint64        `json:"height"`
		OldAddress types.Address `json:"oldAddress"`
		NewAddress types.Address `json:"newAddress"`
	} `json:"hardforkDevAddr"`
	HardforkTax struct {
		Height uint64 `json:"height"`
	} `json:"hardforkTax"`
	HardforkStorageProof struct {
		Height uint64 `json:"height"`
	} `json:"hardforkStorageProof"`
	HardforkOak struct {
		Height           uint64    `json:"height"`
		FixHeight        uint64    `json:"fixHeight"`
		GenesisTimestamp time.Time `json:"genesisTimestamp"`
	} `json:"hardforkOak"`
	HardforkASIC struct {
		Height    uint64        `json:"height"`
		OakTime   time.Duration `json:"oakTime"`
		OakTarget types.BlockID `json:"oakTarget"`
	} `json:"hardforkASIC"`
	HardforkFoundation struct {
		Height          uint64        `json:"height"`
		PrimaryAddress  types.Address `json:"primaryAddress"`
		FailsafeAddress types.Address `json:"failsafeAddress"`
	} `json:"hardforkFoundation"`
	HardforkV2 struct {
		AllowHeight   uint64 `json:"allowHeight"`
		RequireHeight uint64 `json:"requireHeight"`
	} `json:"hardforkV2"`
}

// GenesisState returns the state to which the genesis block should be applied.
func (n *Network) GenesisState() State {
	return State{
		Network: n,

		Index:          types.ChainIndex{Height: ^uint64(0)},
		PrevTimestamps: [11]time.Time{},
		Depth:          intToTarget(maxTarget),
		ChildTarget:    n.InitialTarget,
		SiafundPool:    types.ZeroCurrency,

		OakTime:   0,
		OakTarget: intToTarget(maxTarget),

		FoundationPrimaryAddress:  n.HardforkFoundation.PrimaryAddress,
		FoundationFailsafeAddress: n.HardforkFoundation.FailsafeAddress,
	}
}

// State represents the state of the chain as of a particular block.
type State struct {
	Network *Network `json:"-"` // network parameters are not encoded

	Index          types.ChainIndex `json:"index"`
	PrevTimestamps [11]time.Time    `json:"prevTimestamps"`
	Depth          types.BlockID    `json:"depth"`
	ChildTarget    types.BlockID    `json:"childTarget"`
	SiafundPool    types.Currency   `json:"siafundPool"`

	// hardfork-related state
	OakTime                   time.Duration `json:"oakTime"`
	OakTarget                 types.BlockID `json:"oakTarget"`
	FoundationPrimaryAddress  types.Address `json:"foundationPrimaryAddress"`
	FoundationFailsafeAddress types.Address `json:"foundationFailsafeAddress"`

	Elements     ElementAccumulator `json:"elements"`
	Attestations uint64             `json:"attestations"`
}

// EncodeTo implements types.EncoderTo.
func (s State) EncodeTo(e *types.Encoder) {
	s.Index.EncodeTo(e)
	for _, ts := range s.PrevTimestamps {
		e.WriteTime(ts)
	}
	s.Depth.EncodeTo(e)
	s.ChildTarget.EncodeTo(e)
	s.SiafundPool.EncodeTo(e)

	e.WriteUint64(uint64(s.OakTime))
	s.OakTarget.EncodeTo(e)
	s.FoundationPrimaryAddress.EncodeTo(e)
	s.FoundationFailsafeAddress.EncodeTo(e)
	s.Elements.EncodeTo(e)
	e.WriteUint64(s.Attestations)
}

// DecodeFrom implements types.DecoderFrom.
func (s *State) DecodeFrom(d *types.Decoder) {
	s.Index.DecodeFrom(d)
	for i := range s.PrevTimestamps {
		s.PrevTimestamps[i] = d.ReadTime()
	}
	s.Depth.DecodeFrom(d)
	s.ChildTarget.DecodeFrom(d)
	s.SiafundPool.DecodeFrom(d)

	s.OakTime = time.Duration(d.ReadUint64())
	s.OakTarget.DecodeFrom(d)
	s.FoundationPrimaryAddress.DecodeFrom(d)
	s.FoundationFailsafeAddress.DecodeFrom(d)
	s.Elements.DecodeFrom(d)
	s.Attestations = d.ReadUint64()
}

func (s State) childHeight() uint64 { return s.Index.Height + 1 }

func (s State) numTimestamps() int {
	if s.childHeight() < uint64(len(s.PrevTimestamps)) {
		return int(s.childHeight())
	}
	return len(s.PrevTimestamps)
}

func (s State) medianTimestamp() time.Time {
	prevCopy := s.PrevTimestamps
	ts := prevCopy[:s.numTimestamps()]
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	if len(ts)%2 != 0 {
		return ts[len(ts)/2]
	}
	l, r := ts[len(ts)/2-1], ts[len(ts)/2]
	return l.Add(r.Sub(l) / 2)
}

// MaxFutureTimestamp returns the maximum allowed timestamp for a block.
func (s State) MaxFutureTimestamp(currentTime time.Time) time.Time {
	return currentTime.Add(3 * time.Hour)
}

// BlockInterval is the expected wall clock time between consecutive blocks.
func (s State) BlockInterval() time.Duration {
	return 10 * time.Minute
}

// BlockReward returns the reward for mining a child block.
func (s State) BlockReward() types.Currency {
	r, underflow := s.Network.InitialCoinbase.SubWithUnderflow(types.Siacoins(uint32(s.childHeight())))
	if underflow || r.Cmp(s.Network.MinimumCoinbase) < 0 {
		return s.Network.MinimumCoinbase
	}
	return r
}

// MaturityHeight is the height at which various outputs created in the child
// block will "mature" (become spendable).
func (s State) MaturityHeight() uint64 {
	return s.childHeight() + 144
}

// SiafundCount is the number of siafunds in existence.
func (s State) SiafundCount() uint64 {
	return 10000
}

// AncestorDepth is the depth used to determine the target timestamp in the
// pre-Oak difficulty adjustment algorithm.
func (s State) AncestorDepth() uint64 {
	return 1000
}

// FoundationSubsidy returns the Foundation subsidy output for the child block.
// If no subsidy is due, the returned output has a value of zero.
func (s State) FoundationSubsidy() (sco types.SiacoinOutput) {
	sco.Address = s.FoundationPrimaryAddress

	subsidyPerBlock := types.Siacoins(30000)
	const blocksPerYear = 144 * 365
	const blocksPerMonth = blocksPerYear / 12
	hardforkHeight := s.Network.HardforkFoundation.Height
	if s.childHeight() < hardforkHeight || (s.childHeight()-hardforkHeight)%blocksPerMonth != 0 {
		sco.Value = types.ZeroCurrency
	} else if s.childHeight() == hardforkHeight {
		sco.Value = subsidyPerBlock.Mul64(blocksPerYear)
	} else {
		sco.Value = subsidyPerBlock.Mul64(blocksPerMonth)
	}
	return
}

// NonceFactor is the factor by which all block nonces must be divisible.
func (s State) NonceFactor() uint64 {
	if s.childHeight() < s.Network.HardforkASIC.Height {
		return 1
	}
	return 1009
}

// MaxBlockWeight is the maximum "weight" of a valid child block.
func (s State) MaxBlockWeight() uint64 {
	return 2_000_000
}

type writeCounter struct{ n int }

func (wc *writeCounter) Write(p []byte) (int, error) {
	wc.n += len(p)
	return len(p), nil
}

// TransactionWeight computes the weight of a txn.
func (s State) TransactionWeight(txn types.Transaction) uint64 {
	var wc writeCounter
	e := types.NewEncoder(&wc)
	txn.EncodeTo(e)
	e.Flush()
	return uint64(wc.n)
}

// V2TransactionWeight computes the weight of a txn.
func (s State) V2TransactionWeight(txn types.V2Transaction) uint64 {
	var wc writeCounter
	e := types.NewEncoder(&wc)
	for _, sci := range txn.SiacoinInputs {
		sci.Parent.MerkleProof = nil
		sci.EncodeTo(e)
	}
	for _, sco := range txn.SiacoinOutputs {
		sco.EncodeTo(e)
	}
	for _, sfi := range txn.SiafundInputs {
		sfi.Parent.MerkleProof = nil
		sfi.EncodeTo(e)
	}
	for _, sfo := range txn.SiafundOutputs {
		sfo.EncodeTo(e)
	}
	for _, fc := range txn.FileContracts {
		fc.EncodeTo(e)
	}
	for _, fcr := range txn.FileContractRevisions {
		fcr.Parent.MerkleProof = nil
		fcr.EncodeTo(e)
	}
	for _, fcr := range txn.FileContractResolutions {
		fcr.Parent.MerkleProof = nil
		if sp, ok := fcr.Resolution.(types.V2StorageProof); ok {
			sp.ProofStart.MerkleProof = nil
			fcr.Resolution = sp
		}
		fcr.EncodeTo(e)
	}
	for _, a := range txn.Attestations {
		a.EncodeTo(e)
	}
	e.WriteBytes(txn.ArbitraryData)
	storage := uint64(wc.n)

	var signatures int
	for _, sci := range txn.SiacoinInputs {
		signatures += len(sci.Signatures)
	}
	for _, sfi := range txn.SiafundInputs {
		signatures += len(sfi.Signatures)
	}
	signatures += 2 * len(txn.FileContracts)
	signatures += 2 * len(txn.FileContractRevisions)
	for _, fcr := range txn.FileContractResolutions {
		switch fcr.Resolution.(type) {
		case types.V2FileContractRenewal, types.V2FileContract:
			signatures += 2
		}
	}
	signatures += len(txn.Attestations)

	// TODO: choose coefficients empirically
	return storage + 100*uint64(signatures)
}

// FileContractTax computes the tax levied on a given contract.
func (s State) FileContractTax(fc types.FileContract) types.Currency {
	// multiply by tax rate
	i := fc.Payout.Big()
	if s.childHeight() < s.Network.HardforkTax.Height {
		r := new(big.Rat).SetInt(i)
		r.Mul(r, new(big.Rat).SetFloat64(0.039))
		i.Div(r.Num(), r.Denom())
	} else {
		i.Mul(i, big.NewInt(39))
		i.Div(i, big.NewInt(1000))
	}

	// round down to multiple of SiafundCount
	i.Sub(i, new(big.Int).Mod(i, big.NewInt(int64(s.SiafundCount()))))

	// convert to currency
	lo := i.Uint64()
	hi := i.Rsh(i, 64).Uint64()
	return types.NewCurrency(lo, hi)
}

// V2FileContractTax computes the tax levied on a given v2 contract.
func (s State) V2FileContractTax(fc types.V2FileContract) types.Currency {
	sum := fc.RenterOutput.Value.Add(fc.HostOutput.Value)
	tax := sum.Div64(25) // 4%
	// round down to nearest multiple of SiafundCount
	_, r := bits.Div64(0, tax.Hi, s.SiafundCount())
	_, r = bits.Div64(r, tax.Lo, s.SiafundCount())
	return tax.Sub(types.NewCurrency64(r))
}

// StorageProofLeafIndex returns the leaf index used when computing or
// validating a storage proof.
func (s State) StorageProofLeafIndex(filesize uint64, windowID types.BlockID, fcid types.FileContractID) uint64 {
	const leafSize = uint64(len(types.StorageProof{}.Leaf))
	numLeaves := filesize / leafSize
	if filesize%leafSize != 0 {
		numLeaves++
	}
	if numLeaves == 0 {
		return 0
	}
	seed := types.HashBytes(append(windowID[:], fcid[:]...))
	var r uint64
	for i := 0; i < len(seed); i += 8 {
		_, r = bits.Div64(r, binary.BigEndian.Uint64(seed[i:]), numLeaves)
	}
	return r
}

// StorageProofLeafHash computes the leaf hash of file contract data. If
// len(leaf) < 64, it will be extended with zeros.
func (s State) StorageProofLeafHash(leaf []byte) types.Hash256 {
	const leafSize = len(types.StorageProof{}.Leaf)
	buf := make([]byte, 1+leafSize)
	buf[0] = leafHashPrefix
	copy(buf[1:], leaf)
	return types.HashBytes(buf)
}

// replayPrefix returns the replay protection prefix at the current height.
// These prefixes are included in a transaction's SigHash; a new prefix is used
// after each hardfork to prevent replay attacks.
func (s State) replayPrefix() []byte {
	switch {
	case s.Index.Height >= s.Network.HardforkV2.AllowHeight:
		return []byte{2}
	case s.Index.Height >= s.Network.HardforkFoundation.Height:
		return []byte{1}
	case s.Index.Height >= s.Network.HardforkASIC.Height:
		return []byte{0}
	default:
		return nil
	}
}

// v2ReplayPrefix returns the replay protection prefix at the current height.
// These prefixes are included in various hashes; a new prefix is used after
// each hardfork to prevent replay attacks.
func (s State) v2ReplayPrefix() uint8 {
	return 2
}

// WholeSigHash computes the hash of transaction data covered by the
// WholeTransaction flag.
func (s State) WholeSigHash(txn types.Transaction, parentID types.Hash256, pubkeyIndex uint64, timelock uint64, coveredSigs []uint64) types.Hash256 {
	h := hasherPool.Get().(*types.Hasher)
	defer hasherPool.Put(h)
	h.Reset()

	h.E.WritePrefix(len((txn.SiacoinInputs)))
	for i := range txn.SiacoinInputs {
		h.E.Write(s.replayPrefix())
		txn.SiacoinInputs[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.SiacoinOutputs)))
	for i := range txn.SiacoinOutputs {
		txn.SiacoinOutputs[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.FileContracts)))
	for i := range txn.FileContracts {
		txn.FileContracts[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.FileContractRevisions)))
	for i := range txn.FileContractRevisions {
		txn.FileContractRevisions[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.StorageProofs)))
	for i := range txn.StorageProofs {
		txn.StorageProofs[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.SiafundInputs)))
	for i := range txn.SiafundInputs {
		h.E.Write(s.replayPrefix())
		txn.SiafundInputs[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.SiafundOutputs)))
	for i := range txn.SiafundOutputs {
		txn.SiafundOutputs[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.MinerFees)))
	for i := range txn.MinerFees {
		txn.MinerFees[i].EncodeTo(h.E)
	}
	h.E.WritePrefix(len((txn.ArbitraryData)))
	for i := range txn.ArbitraryData {
		h.E.WriteBytes(txn.ArbitraryData[i])
	}

	parentID.EncodeTo(h.E)
	h.E.WriteUint64(pubkeyIndex)
	h.E.WriteUint64(timelock)

	for _, i := range coveredSigs {
		txn.Signatures[i].EncodeTo(h.E)
	}

	return h.Sum()
}

// PartialSigHash computes the hash of the transaction data specified by cf. It
// panics if cf references fields not present in txn.
func (s State) PartialSigHash(txn types.Transaction, cf types.CoveredFields) types.Hash256 {
	h := hasherPool.Get().(*types.Hasher)
	defer hasherPool.Put(h)
	h.Reset()

	for _, i := range cf.SiacoinInputs {
		h.E.Write(s.replayPrefix())
		txn.SiacoinInputs[i].EncodeTo(h.E)
	}
	for _, i := range cf.SiacoinOutputs {
		txn.SiacoinOutputs[i].EncodeTo(h.E)
	}
	for _, i := range cf.FileContracts {
		txn.FileContracts[i].EncodeTo(h.E)
	}
	for _, i := range cf.FileContractRevisions {
		txn.FileContractRevisions[i].EncodeTo(h.E)
	}
	for _, i := range cf.StorageProofs {
		txn.StorageProofs[i].EncodeTo(h.E)
	}
	for _, i := range cf.SiafundInputs {
		h.E.Write(s.replayPrefix())
		txn.SiafundInputs[i].EncodeTo(h.E)
	}
	for _, i := range cf.SiafundOutputs {
		txn.SiafundOutputs[i].EncodeTo(h.E)
	}
	for _, i := range cf.MinerFees {
		txn.MinerFees[i].EncodeTo(h.E)
	}
	for _, i := range cf.ArbitraryData {
		h.E.WriteBytes(txn.ArbitraryData[i])
	}
	for _, i := range cf.Signatures {
		txn.Signatures[i].EncodeTo(h.E)
	}

	return h.Sum()
}

// Commitment computes the commitment hash for a child block.
func (s State) Commitment(minerAddr types.Address, txns []types.Transaction, v2txns []types.V2Transaction) types.Hash256 {
	h := hasherPool.Get().(*types.Hasher)
	defer hasherPool.Put(h)
	h.Reset()

	// hash the state
	s.EncodeTo(h.E)
	stateHash := h.Sum()

	// hash the transactions
	var acc blake2b.Accumulator
	for _, txn := range txns {
		h.Reset()
		txn.EncodeTo(h.E)
		acc.AddLeaf(h.Sum())
	}
	for _, txn := range v2txns {
		h.Reset()
		txn.EncodeTo(h.E)
		acc.AddLeaf(h.Sum())
	}
	txnsHash := types.Hash256(acc.Root())

	// concatenate the hashes and the miner address
	h.Reset()
	h.E.WriteString("sia/commitment|")
	h.E.WriteUint8(s.v2ReplayPrefix())
	stateHash.EncodeTo(h.E)
	minerAddr.EncodeTo(h.E)
	txnsHash.EncodeTo(h.E)
	return h.Sum()
}

// InputSigHash returns the hash that must be signed for each v2 transaction input.
func (s State) InputSigHash(txn types.V2Transaction) types.Hash256 {
	// NOTE: This currently covers exactly the same fields as txn.ID(), and for
	// similar reasons.
	h := hasherPool.Get().(*types.Hasher)
	defer hasherPool.Put(h)
	h.Reset()
	h.E.WriteString("sia/id/transaction|")
	h.E.WriteUint8(s.v2ReplayPrefix())
	h.E.WritePrefix(len(txn.SiacoinInputs))
	for _, in := range txn.SiacoinInputs {
		in.Parent.ID.EncodeTo(h.E)
	}
	h.E.WritePrefix(len(txn.SiacoinOutputs))
	for _, out := range txn.SiacoinOutputs {
		out.EncodeTo(h.E)
	}
	h.E.WritePrefix(len(txn.SiafundInputs))
	for _, in := range txn.SiafundInputs {
		in.Parent.ID.EncodeTo(h.E)
	}
	h.E.WritePrefix(len(txn.SiafundOutputs))
	for _, out := range txn.SiafundOutputs {
		out.EncodeTo(h.E)
	}
	h.E.WritePrefix(len(txn.FileContracts))
	for _, fc := range txn.FileContracts {
		fc.EncodeTo(h.E)
	}
	h.E.WritePrefix(len(txn.FileContractRevisions))
	for _, fcr := range txn.FileContractRevisions {
		fcr.Parent.ID.EncodeTo(h.E)
		fcr.Revision.EncodeTo(h.E)
	}
	h.E.WritePrefix(len(txn.FileContractResolutions))
	for _, fcr := range txn.FileContractResolutions {
		fcr.Parent.ID.EncodeTo(h.E)
		// normalize proof
		if sp, ok := fcr.Resolution.(types.V2StorageProof); ok {
			sp.ProofStart.MerkleProof = nil
			fcr.Resolution = sp
		}
		fcr.Resolution.(types.EncoderTo).EncodeTo(h.E)
	}
	for _, a := range txn.Attestations {
		a.EncodeTo(h.E)
	}
	h.E.WriteBytes(txn.ArbitraryData)
	h.E.WriteBool(txn.NewFoundationAddress != nil)
	if txn.NewFoundationAddress != nil {
		txn.NewFoundationAddress.EncodeTo(h.E)
	}
	txn.MinerFee.EncodeTo(h.E)
	return h.Sum()
}

// ContractSigHash returns the hash that must be signed for a v2 contract revision.
func (s State) ContractSigHash(fc types.V2FileContract) types.Hash256 {
	h := hasherPool.Get().(*types.Hasher)
	defer hasherPool.Put(h)
	h.Reset()
	h.E.WriteString("sia/sig/filecontract|")
	h.E.WriteUint8(s.v2ReplayPrefix())
	h.E.WriteUint64(fc.Filesize)
	fc.FileMerkleRoot.EncodeTo(h.E)
	h.E.WriteUint64(fc.ProofHeight)
	h.E.WriteUint64(fc.ExpirationHeight)
	fc.RenterOutput.EncodeTo(h.E)
	fc.HostOutput.EncodeTo(h.E)
	fc.MissedHostValue.EncodeTo(h.E)
	fc.RenterPublicKey.EncodeTo(h.E)
	fc.HostPublicKey.EncodeTo(h.E)
	h.E.WriteUint64(fc.RevisionNumber)
	return h.Sum()
}

// RenewalSigHash returns the hash that must be signed for a file contract renewal.
func (s State) RenewalSigHash(fcr types.V2FileContractRenewal) types.Hash256 {
	h := hasherPool.Get().(*types.Hasher)
	defer hasherPool.Put(h)
	h.Reset()
	h.E.WriteString("sia/sig/filecontractrenewal|")
	h.E.WriteUint8(s.v2ReplayPrefix())
	fcr.FinalRevision.EncodeTo(h.E)
	fcr.InitialRevision.EncodeTo(h.E)
	fcr.RenterRollover.EncodeTo(h.E)
	fcr.HostRollover.EncodeTo(h.E)
	return h.Sum()
}

// AttestationSigHash returns the hash that must be signed for an attestation.
func (s State) AttestationSigHash(a types.Attestation) types.Hash256 {
	h := hasherPool.Get().(*types.Hasher)
	defer hasherPool.Put(h)
	h.Reset()
	h.E.WriteString("sia/sig/attestation|")
	h.E.WriteUint8(s.v2ReplayPrefix())
	a.PublicKey.EncodeTo(h.E)
	h.E.WriteString(a.Key)
	h.E.WriteBytes(a.Value)
	return h.Sum()
}

// A V1TransactionSupplement contains elements that are associated with a v1
// transaction, but not included in the transaction. For example, v1
// transactions reference the ID of each SiacoinOutput they spend, but do not
// contain the output itself. Consequently, in order to validate the
// transaction, those outputs must be loaded from a Store. Collecting these
// elements into an explicit struct allows us to preserve them even after the
// Store has been mutated.
type V1TransactionSupplement struct {
	SiacoinInputs        []types.SiacoinElement
	SiafundInputs        []types.SiafundElement
	RevisedFileContracts []types.FileContractElement
	ValidFileContracts   []types.FileContractElement
	StorageProofBlockIDs []types.BlockID // must match ValidFileContracts
}

// EncodeTo implements types.EncoderTo.
func (ts V1TransactionSupplement) EncodeTo(e *types.Encoder) {
	e.WritePrefix(len(ts.SiacoinInputs))
	for i := range ts.SiacoinInputs {
		ts.SiacoinInputs[i].EncodeTo(e)
	}
	e.WritePrefix(len(ts.SiafundInputs))
	for i := range ts.SiafundInputs {
		ts.SiafundInputs[i].EncodeTo(e)
	}
	e.WritePrefix(len(ts.RevisedFileContracts))
	for i := range ts.RevisedFileContracts {
		ts.RevisedFileContracts[i].EncodeTo(e)
	}
	e.WritePrefix(len(ts.ValidFileContracts))
	for i := range ts.ValidFileContracts {
		ts.ValidFileContracts[i].EncodeTo(e)
	}
}

// DecodeFrom implements types.DecoderFrom.
func (ts *V1TransactionSupplement) DecodeFrom(d *types.Decoder) {
	ts.SiacoinInputs = make([]types.SiacoinElement, d.ReadPrefix())
	for i := range ts.SiacoinInputs {
		ts.SiacoinInputs[i].DecodeFrom(d)
	}
	ts.SiafundInputs = make([]types.SiafundElement, d.ReadPrefix())
	for i := range ts.SiafundInputs {
		ts.SiafundInputs[i].DecodeFrom(d)
	}
	ts.RevisedFileContracts = make([]types.FileContractElement, d.ReadPrefix())
	for i := range ts.RevisedFileContracts {
		ts.RevisedFileContracts[i].DecodeFrom(d)
	}
	ts.ValidFileContracts = make([]types.FileContractElement, d.ReadPrefix())
	for i := range ts.ValidFileContracts {
		ts.ValidFileContracts[i].DecodeFrom(d)
	}
}

func (ts V1TransactionSupplement) siacoinElement(id types.SiacoinOutputID) (sce types.SiacoinElement, ok bool) {
	for _, sce := range ts.SiacoinInputs {
		if types.SiacoinOutputID(sce.ID) == id {
			return sce, true
		}
	}
	return
}

func (ts V1TransactionSupplement) siafundElement(id types.SiafundOutputID) (sce types.SiafundElement, ok bool) {
	for _, sfe := range ts.SiafundInputs {
		if types.SiafundOutputID(sfe.ID) == id {
			return sfe, true
		}
	}
	return
}

func (ts V1TransactionSupplement) fileContractElement(id types.FileContractID) (sce types.FileContractElement, ok bool) {
	for _, fce := range ts.RevisedFileContracts {
		if types.FileContractID(fce.ID) == id {
			return fce, true
		}
	}
	for _, fce := range ts.ValidFileContracts {
		if types.FileContractID(fce.ID) == id {
			return fce, true
		}
	}
	return
}

func (ts V1TransactionSupplement) storageProofWindowID(id types.FileContractID) types.BlockID {
	for i, fce := range ts.ValidFileContracts {
		if types.FileContractID(fce.ID) == id {
			return ts.StorageProofBlockIDs[i]
		}
	}
	panic("missing contract for storage proof window ID") // developer error
}

// A V1BlockSupplement contains elements that are associated with a v1 block,
// but not included in the block. This includes supplements for each v1
// transaction, as well as any file contracts that expired at the block's
// height.
type V1BlockSupplement struct {
	Transactions          []V1TransactionSupplement
	ExpiringFileContracts []types.FileContractElement
}

// EncodeTo implements types.EncoderTo.
func (bs V1BlockSupplement) EncodeTo(e *types.Encoder) {
	e.WritePrefix(len(bs.Transactions))
	for i := range bs.Transactions {
		bs.Transactions[i].EncodeTo(e)
	}
	e.WritePrefix(len(bs.ExpiringFileContracts))
	for i := range bs.ExpiringFileContracts {
		bs.ExpiringFileContracts[i].EncodeTo(e)
	}
}

// DecodeFrom implements types.DecoderFrom.
func (bs *V1BlockSupplement) DecodeFrom(d *types.Decoder) {
	bs.Transactions = make([]V1TransactionSupplement, d.ReadPrefix())
	for i := range bs.Transactions {
		bs.Transactions[i].DecodeFrom(d)
	}
	bs.ExpiringFileContracts = make([]types.FileContractElement, d.ReadPrefix())
	for i := range bs.ExpiringFileContracts {
		bs.ExpiringFileContracts[i].DecodeFrom(d)
	}
}
