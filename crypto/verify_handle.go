package crypto

import (
	"bytes"
	"io"
	"io/ioutil"

	"github.com/ProtonMail/go-crypto/v2/openpgp/armor"
	armorHelper "github.com/ProtonMail/gopenpgp/v3/armor"

	"github.com/ProtonMail/go-crypto/v2/openpgp"
	"github.com/ProtonMail/go-crypto/v2/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/v2/openpgp/packet"
	"github.com/ProtonMail/gopenpgp/v3/constants"
	"github.com/ProtonMail/gopenpgp/v3/internal"
	"github.com/pkg/errors"
)

type verifyHandle struct {
	VerifyKeyRing          *KeyRing
	VerificationContext    *VerificationContext
	DisableVerifyTimeCheck bool
	clock                  Clock
}

// --- Default verification handle to build from

func defaultVerifyHandle(clock Clock) *verifyHandle {
	return &verifyHandle{
		clock: clock,
	}
}

// --- Implements VerifyHandle functions

// VerifyingReader wraps a reader with a signature verify reader.
// Once all data is read from the returned verify reader, the signature can be verified
// with (VerifyDataReader).VerifySignature().
// Note that an error is only returned if it is not a signature error.
// If detachedData is nil, signatureMessage is treated as an inline signature message.
// Thus, it is expected that signatureMessage contains the data to be verified.
// If detachedData is not nil, signatureMessage must contain a detached signature,
// which is verified against the detachedData.
func (vh *verifyHandle) VerifyingReader(detachedData, signatureMessage Reader) (*VerifyDataReader, error) {
	var armored bool
	signatureMessage, armored = armorHelper.IsPGPArmored(signatureMessage)
	if armored {
		// Wrap with decode armor reader.
		armoredBlock, err := armor.Decode(signatureMessage)
		if err != nil {
			return nil, errors.Wrap(err, "gopenpgp: unarmor failed")
		}
		signatureMessage = armoredBlock.Body
	}
	if detachedData != nil {
		return vh.verifyingDetachedReader(detachedData, signatureMessage)
	} else {
		return vh.verifyingReader(signatureMessage)
	}
}

// Verify verifies either a inline signature message or a detached signature
// and returns a VerifyResult. The VerifyResult can be checked for failure
// and allows access to information about the signature.
// Note that an error is only returned if it is not a signature error.
// If detachedData is nil, signatureMessage is treated as an inline signature message.
// Thus, it is expected that signatureMessage contains the data to be verified.
// If detachedData is not nil, signatureMessage must contain a detached signature,
// which is verified against the detachedData.
func (vh *verifyHandle) Verify(detachedData, signatureMessage []byte) (verifyResult *VerifiedDataResult, err error) {
	var ptReader *VerifyDataReader
	signatureMessageReader := bytes.NewReader(signatureMessage)
	if detachedData != nil {
		detachedDataReader := bytes.NewReader(detachedData)
		ptReader, err = vh.VerifyingReader(detachedDataReader, signatureMessageReader)
	} else {
		ptReader, err = vh.VerifyingReader(nil, signatureMessageReader)
	}

	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: verifying signature failed")
	}
	var data []byte
	if detachedData != nil {
		_, err = io.Copy(ioutil.Discard, ptReader)
	} else {
		data, err = ptReader.ReadAll()
	}
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: reading data to verify signature failed")
	}
	sigVerifyResult, err := ptReader.VerifySignature()
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: verifying signature failed")
	}
	return &VerifiedDataResult{
		data:         data,
		metadata:     ptReader.GetMetadata(),
		VerifyResult: *sigVerifyResult,
	}, nil
}

// VerifyCleartext verifies an armored cleartext message
// and returns a VerifyCleartextResult. The VerifyCleartextResult can be checked for failure
// and allows access the contained message
// Note that an error is only returned if it is not a signature error.
func (vh *verifyHandle) VerifyCleartext(cleartext []byte) (*VerifyCleartextResult, error) {
	return vh.verifyCleartext(cleartext)
}

// --- Private logic functions

func (vh *verifyHandle) validate() error {
	if vh.VerifyKeyRing == nil {
		return errors.New("gopenpgp: no verification key provided")
	}
	return nil
}

// verifySignature verifies if a signature is valid with the entity list.
func (vh *verifyHandle) verifyDetachedSignature(
	origText io.Reader,
	signature []byte,
) (result *VerifyResult, err error) {
	signatureReader := bytes.NewReader(signature)
	ptReader, err := vh.verifyingDetachedReader(origText, signatureReader)
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: verify signature failed")
	}
	_, err = io.Copy(ioutil.Discard, ptReader)
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: hashing failed")
	}
	return ptReader.VerifySignature()
}

func (vh *verifyHandle) verifyingReader(
	signatureMessage io.Reader,
) (reader *VerifyDataReader, err error) {
	config := &packet.Config{}
	verifyTime := vh.clock().Unix()
	config.Time = NewConstantClock(verifyTime)
	if vh.VerificationContext != nil {
		config.KnownNotations = map[string]bool{constants.SignatureContextName: true}
	}
	md, err := openpgp.ReadMessage(
		signatureMessage,
		vh.VerifyKeyRing.getEntities(),
		nil,
		config,
	)
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: initialize signature reader failed")
	}
	return &VerifyDataReader{
		md,
		md.UnverifiedBody,
		vh.VerifyKeyRing,
		verifyTime,
		vh.DisableVerifyTimeCheck,
		false,
		vh.VerificationContext,
	}, nil
}

func (vh *verifyHandle) verifyingDetachedReader(
	data Reader,
	signature Reader,
) (*VerifyDataReader, error) {
	return verifyingDetachedReader(
		data,
		signature,
		vh.VerifyKeyRing,
		vh.VerificationContext,
		vh.DisableVerifyTimeCheck,
		nil,
		vh.clock,
	)
}

func (vh *verifyHandle) verifyCleartext(cleartext []byte) (*VerifyCleartextResult, error) {
	block, _ := clearsign.Decode(cleartext)
	if block == nil {
		return nil, errors.New("gopenpgp: not able to parse cleartext message")
	}
	signature, err := io.ReadAll(block.ArmoredSignature.Body)
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: signature not parsable in cleartext")
	}
	if err != nil {
		return nil, errors.New("gopenpgp: cleartext header not parsable")
	}
	reader := bytes.NewReader(block.Bytes)
	result, err := vh.verifyDetachedSignature(
		reader,
		signature,
	)
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: cleartext verify failed with non-signature error")
	}
	return &VerifyCleartextResult{
		VerifyResult: *result,
		cleartext:    block.Bytes,
	}, nil
}

func verifyingDetachedReader(
	data Reader,
	signature Reader,
	verifyKeyRing *KeyRing,
	verificationContext *VerificationContext,
	disableVerifyTimeCheck bool,
	config *packet.Config,
	clock Clock,
) (*VerifyDataReader, error) {
	if config == nil {
		config = &packet.Config{}
	}
	verifyTime := clock().Unix()
	config.Time = NewConstantClock(verifyTime)
	if verificationContext != nil {
		config.KnownNotations = map[string]bool{constants.SignatureContextName: true}
	}
	md, err := openpgp.VerifyDetachedSignatureReader(
		verifyKeyRing.getEntities(),
		data,
		signature,
		config,
	)
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: verify signature reader failed")
	}
	internalReader := md.UnverifiedBody
	if len(md.SignatureCandidates) > 0 &&
		md.SignatureCandidates[0].SigType == packet.SigTypeText {
		internalReader = internal.NewSanitizeReader(internalReader)
	}
	return &VerifyDataReader{
		md,
		internalReader,
		verifyKeyRing,
		verifyTime,
		disableVerifyTimeCheck,
		false,
		verificationContext,
	}, nil
}
