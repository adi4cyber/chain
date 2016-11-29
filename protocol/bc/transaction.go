package bc

import (
	"bytes"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"io"

	"chain-stealth/crypto/ca"
	"chain-stealth/crypto/sha3pool"
	"chain-stealth/encoding/blockchain"
	"chain-stealth/encoding/bufpool"
	"chain-stealth/errors"
)

// CurrentTransactionVersion is the current latest
// supported transaction version.
const CurrentTransactionVersion = 2

// Tx holds a transaction along with its hash.
type Tx struct {
	TxData
	Hash Hash
}

func (tx *Tx) UnmarshalText(p []byte) error {
	if err := tx.TxData.UnmarshalText(p); err != nil {
		return err
	}

	tx.Hash = tx.TxData.Hash()
	return nil
}

// NewTx returns a new Tx containing data and its hash.
// If you have already computed the hash, use struct literal
// notation to make a Tx object directly.
func NewTx(data TxData) *Tx {
	return &Tx{
		TxData: data,
		Hash:   data.Hash(),
	}
}

// These flags are part of the wire protocol;
// they must not change.
const (
	SerWitness uint8 = 1 << iota
	SerPrevout
	SerMetadata

	// Bit mask for accepted serialization flags.
	// All other flag bits must be 0.
	SerValid    = 0x7
	serRequired = 0x7 // we support only this combination of flags
)

// WitnessHash is the combined hash of the
// transactions hash and signature data hash.
// It is used to compute the TxRoot of a block.
func (tx *Tx) WitnessHash() (hash Hash, err error) {
	hasher := sha3pool.Get256()
	defer sha3pool.Put256(hasher)

	hasher.Write(tx.Hash[:])

	cwhash, err := tx.commonWitnessHash()
	if err != nil {
		return hash, err
	}
	hasher.Write(cwhash[:])

	blockchain.WriteVarint31(hasher, uint64(len(tx.Inputs))) // TODO(bobg): check and return error
	for _, txin := range tx.Inputs {
		h, err := txin.WitnessHash()
		if err != nil {
			return hash, err
		}
		hasher.Write(h[:])
	}

	blockchain.WriteVarint31(hasher, uint64(len(tx.Outputs))) // TODO(bobg): check and return error
	for _, txout := range tx.Outputs {
		h, err := txout.WitnessHash()
		if err != nil {
			return hash, err
		}
		hasher.Write(h[:])
	}

	hasher.Read(hash[:])
	return hash, nil
}

// TxData encodes a transaction in the blockchain.
// Most users will want to use Tx instead;
// it includes the hash.
type TxData struct {
	Version           uint64
	Inputs            []*TxInput
	Outputs           []*TxOutput
	MinTime           uint64
	MaxTime           uint64
	ReferenceData     []byte
	ExcessCommitments []ca.ExcessCommitment // asset v2 only
}

// HasIssuance returns true if this transaction has an issuance input.
func (tx *TxData) HasIssuance() bool {
	for _, in := range tx.Inputs {
		if in.IsIssuance() {
			return true
		}
	}
	return false
}

func (tx *TxData) UnmarshalText(p []byte) error {
	b := make([]byte, hex.DecodedLen(len(p)))
	_, err := hex.Decode(b, p)
	if err != nil {
		return err
	}
	return tx.readFrom(bytes.NewReader(b))
}

func (tx *TxData) Scan(val interface{}) error {
	b, ok := val.([]byte)
	if !ok {
		return errors.New("Scan must receive a byte slice")
	}
	return tx.readFrom(bytes.NewReader(b))
}

func (tx *TxData) Value() (driver.Value, error) {
	b := new(bytes.Buffer)
	_, err := tx.WriteTo(b)
	if err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// WriteTo writes tx to w.
func (tx *TxData) WriteTo(w io.Writer) (int64, error) {
	ew := errors.NewWriter(w)
	tx.writeTo(ew, serRequired)
	return ew.Written(), ew.Err()
}

func (tx *TxData) writeTo(w io.Writer, serflags byte) error {
	_, err := w.Write([]byte{serflags})
	if err != nil {
		return err
	}
	_, err = blockchain.WriteVarint63(w, tx.Version)
	if err != nil {
		return err
	}

	_, err = blockchain.WriteExtensibleString(w, func(w io.Writer) error {
		_, err := blockchain.WriteVarint63(w, tx.MinTime)
		if err != nil {
			return err
		}
		_, err = blockchain.WriteVarint63(w, tx.MaxTime)
		return err
	})
	if err != nil {
		return err
	}

	// common witness
	_, err = blockchain.WriteExtensibleString(w, func(w io.Writer) error {
		return tx.writeCommonWitness(w)
	})
	if err != nil {
		return err
	}

	_, err = blockchain.WriteVarint31(w, uint64(len(tx.Inputs)))
	if err != nil {
		return err
	}
	for _, ti := range tx.Inputs {
		err = ti.writeTo(w, serflags)
		if err != nil {
			return err
		}
	}

	_, err = blockchain.WriteVarint31(w, uint64(len(tx.Outputs)))
	if err != nil {
		return err
	}
	for _, to := range tx.Outputs {
		err = to.writeTo(w, serflags)
		if err != nil {
			return err
		}
	}

	return writeRefData(w, tx.ReferenceData, serflags)
}

// does not write the enclosing extensible string
func (tx *TxData) writeCommonWitness(w io.Writer) error {
	if tx.Version == 2 {
		_, err := blockchain.WriteVarint31(w, uint64(len(tx.ExcessCommitments)))
		if err != nil {
			return err
		}
		for _, lc := range tx.ExcessCommitments {
			err = lc.WriteTo(w)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (tx *TxData) readFrom(r io.Reader) error {
	var serflags [1]byte
	_, err := io.ReadFull(r, serflags[:])
	if err != nil {
		return errors.Wrap(err, "reading serialization flags")
	}
	if err == nil && serflags[0] != serRequired {
		return fmt.Errorf("unsupported serflags %#x", serflags[0])
	}

	tx.Version, _, err = blockchain.ReadVarint63(r)
	if err != nil {
		return errors.Wrap(err, "reading transaction version")
	}

	// Common fields
	all := tx.Version == 1 || tx.Version == 2
	_, err = blockchain.ReadExtensibleString(r, all, func(r io.Reader) error {
		tx.MinTime, _, err = blockchain.ReadVarint63(r)
		if err != nil {
			return errors.Wrap(err, "reading transaction mintime")
		}
		tx.MaxTime, _, err = blockchain.ReadVarint63(r)
		return errors.Wrap(err, "reading transaction maxtime")
	})
	if err != nil {
		return errors.Wrap(err, "reading transaction common fields")
	}

	// Common witness
	_, err = blockchain.ReadExtensibleString(r, false, func(r io.Reader) error {
		return tx.readCommonWitness(r)
	})
	if err != nil {
		return errors.Wrap(err, "reading transaction common witness")
	}

	n, _, err := blockchain.ReadVarint31(r)
	if err != nil {
		return errors.Wrap(err, "reading number of inputs")
	}
	for ; n > 0; n-- {
		ti := new(TxInput)
		err = ti.readFrom(r, tx.Version)
		if err != nil {
			return errors.Wrapf(err, "reading input %d", len(tx.Inputs))
		}
		tx.Inputs = append(tx.Inputs, ti)
	}

	n, _, err = blockchain.ReadVarint31(r)
	if err != nil {
		return errors.Wrap(err, "reading number of outputs")
	}
	for ; n > 0; n-- {
		to := new(TxOutput)
		err = to.readFrom(r, tx.Version)
		if err != nil {
			return errors.Wrapf(err, "reading output %d", len(tx.Outputs))
		}
		tx.Outputs = append(tx.Outputs, to)
	}

	tx.ReferenceData, _, err = blockchain.ReadVarstr31(r)
	return errors.Wrap(err, "reading transaction reference data")
}

// tx.Version must be initialized
// does not read the enclosing extensible string
func (tx *TxData) readCommonWitness(r io.Reader) error {
	if tx.Version == 2 {
		n, _, err := blockchain.ReadVarint31(r)
		if err != nil {
			return errors.Wrap(err, "reading number of excess commitments")
		}
		tx.ExcessCommitments = make([]ca.ExcessCommitment, n)
		for i := uint32(0); i < n; i++ {
			err = tx.ExcessCommitments[i].ReadFrom(r)
			if err != nil {
				return errors.Wrapf(err, "reading excess commitment %d", i)
			}
		}
	}
	return nil
}

func (tx *TxData) commonWitnessHash() (h Hash, err error) {
	hasher := sha3pool.Get256()
	defer sha3pool.Put256(hasher)

	buf := bufpool.Get()
	defer bufpool.Put(buf)

	err = tx.writeCommonWitness(buf)
	if err != nil {
		return h, err
	}

	hasher.Write(buf.Bytes())
	hasher.Read(h[:])
	return h, nil
}

// Hash computes the hash of the transaction with reference data fields
// replaced by their hashes,
// and stores the result in Hash.
func (tx *TxData) Hash() Hash {
	h := sha3pool.Get256()
	tx.writeTo(h, 0) // error is impossible
	var v Hash
	h.Read(v[:])
	sha3pool.Put256(h)
	return v
}

func (tx *TxData) IssuanceHash(n int) (h Hash, err error) {
	if n < 0 || n >= len(tx.Inputs) {
		return h, fmt.Errorf("no input %d", n)
	}

	t := tx.Inputs[n]

	nonce, ok := t.Nonce()
	if !ok {
		return h, fmt.Errorf("not an issuance input")
	}

	buf := sha3pool.Get256()
	defer sha3pool.Put256(buf)

	_, err = blockchain.WriteVarstr31(buf, nonce)
	if err != nil {
		return h, err
	}

	switch inp := t.TypedInput.(type) {
	case *IssuanceInput1:
		assetID := inp.AssetID()
		buf.Write(assetID[:])
	case *IssuanceInput2:
		err = inp.assetDescriptor.WriteTo(buf)
		if err != nil {
			return h, err
		}
	}

	_, err = blockchain.WriteVarint63(buf, tx.MinTime)
	if err != nil {
		return h, err
	}

	_, err = blockchain.WriteVarint63(buf, tx.MaxTime)
	if err != nil {
		return h, err
	}

	buf.Read(h[:])
	return h, nil
}

// HashForSig generates the hash required for the specified input's
// signature.
func (tx *TxData) HashForSig(idx int) Hash {
	return NewSigHasher(tx).Hash(idx)
}

func (tx *TxData) MarshalText() ([]byte, error) {
	var buf bytes.Buffer
	tx.WriteTo(&buf) // error is impossible
	b := make([]byte, hex.EncodedLen(buf.Len()))
	hex.Encode(b, buf.Bytes())
	return b, nil
}

// assumes w has sticky errors
func writeRefData(w io.Writer, data []byte, serflags byte) error {
	if serflags&SerMetadata != 0 {
		_, err := blockchain.WriteVarstr31(w, data)
		return err
	}
	return WriteFastHash(w, data)
}
