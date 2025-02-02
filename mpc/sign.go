package mpc

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/anyswap/CrossChain-Router/v3/common"
	"github.com/anyswap/CrossChain-Router/v3/log"
	"github.com/anyswap/CrossChain-Router/v3/mongodb"
	"github.com/anyswap/CrossChain-Router/v3/tools/crypto"
	"github.com/anyswap/CrossChain-Router/v3/tools/keystore"
	"github.com/anyswap/CrossChain-Router/v3/tools/rlp"
	"github.com/anyswap/CrossChain-Router/v3/types"
)

const (
	pingCount     = 3
	retrySignLoop = 3
)

var (
	errSignTimerTimeout     = errors.New("sign timer timeout")
	errDoSignFailed         = errors.New("do sign failed")
	errSignWithoutPublickey = errors.New("sign without public key")
	errGetSignResultFailed  = errors.New("get sign result failed")
	errRValueIsUsed         = errors.New("r value is already used")
	errWrongSignatureLength = errors.New("wrong signature length")
	errNoUsableSignGroups   = errors.New("no usable sign groups")

	// delete if fail too many times consecutively, 0 means disable checking
	maxSignGroupFailures      = 0
	minIntervalToAddSignGroup = int64(3600)                   // seconds
	signGroupFailuresMap      = make(map[string]signFailures) // key is groupID
)

type signFailures struct {
	count    int
	lastTime int64
}

func pingMPCNode(nodeInfo *NodeInfo) (err error) {
	rpcAddr := nodeInfo.mpcRPCAddress
	for j := 0; j < pingCount; j++ {
		_, err = GetEnode(rpcAddr)
		if err == nil {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	log.Error("pingMPCNode failed", "rpcAddr", rpcAddr, "pingCount", pingCount, "err", err)
	return err
}

// DoSignOne mpc sign single msgHash with context msgContext
func DoSignOne(signPubkey, msgHash, msgContext string) (keyID string, rsvs []string, err error) {
	return DoSign(signPubkey, []string{msgHash}, []string{msgContext})
}

// DoSign mpc sign msgHash with context msgContext
func DoSign(signPubkey string, msgHash, msgContext []string) (keyID string, rsvs []string, err error) {
	log.Debug("mpc DoSign", "msgHash", msgHash, "msgContext", msgContext)
	if signPubkey == "" {
		return "", nil, errSignWithoutPublickey
	}
	for i := 0; i < retrySignLoop; i++ {
		for _, mpcNode := range allInitiatorNodes {
			if err = pingMPCNode(mpcNode); err != nil {
				continue
			}
			signGroupIndexes := mpcNode.getUsableSignGroupIndexes()
			signGroupsCount := int64(len(signGroupIndexes))
			if signGroupsCount == 0 {
				err = errNoUsableSignGroups
				continue
			}
			// randomly pick first subgroup to sign
			randIndex, _ := rand.Int(rand.Reader, big.NewInt(signGroupsCount))
			startIndex := randIndex.Int64()
			i := startIndex
			for {
				keyID, rsvs, err = doSignImpl(mpcNode, signGroupIndexes[i], signPubkey, msgHash, msgContext)
				if err == nil {
					return keyID, rsvs, nil
				}
				i = (i + 1) % signGroupsCount
				if i == startIndex {
					break
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	log.Warn("mpc DoSign failed", "msgHash", msgHash, "msgContext", msgContext, "err", err)
	return "", nil, errDoSignFailed
}

func doSignImpl(mpcNode *NodeInfo, signGroupIndex int, signPubkey string, msgHash, msgContext []string) (keyID string, rsvs []string, err error) {
	nonce, err := GetSignNonce(mpcNode.mpcUser.String(), mpcNode.mpcRPCAddress)
	if err != nil {
		return "", nil, err
	}
	signGroup := mpcNode.originSignGroups[signGroupIndex]
	txdata := SignData{
		TxType:     "SIGN",
		PubKey:     signPubkey,
		MsgHash:    msgHash,
		MsgContext: msgContext,
		Keytype:    "ECDSA",
		GroupID:    signGroup,
		ThresHold:  mpcThreshold,
		Mode:       mpcMode,
		TimeStamp:  common.NowMilliStr(),
	}
	payload, _ := json.Marshal(txdata)
	rawTX, err := BuildMPCRawTx(nonce, payload, mpcNode.keyWrapper)
	if err != nil {
		return "", nil, err
	}

	rpcAddr := mpcNode.mpcRPCAddress
	keyID, err = Sign(rawTX, rpcAddr)
	if err != nil {
		return "", nil, err
	}

	rsvs, err = getSignResult(keyID, rpcAddr)
	if err != nil {
		if maxSignGroupFailures > 0 {
			old := signGroupFailuresMap[signGroup]
			signGroupFailuresMap[signGroup] = signFailures{
				count:    old.count + 1,
				lastTime: time.Now().Unix(),
			}
			if old.count+1 >= maxSignGroupFailures {
				log.Error("delete sign group as consecutive failures", "signGroup", signGroup)
				mpcNode.deleteSignGroup(signGroupIndex)
			}
		}
		return "", nil, err
	}
	if maxSignGroupFailures > 0 {
		// reset when succeed
		signGroupFailuresMap[signGroup] = signFailures{
			count:    0,
			lastTime: time.Now().Unix(),
		}
	}
	for _, rsv := range rsvs {
		signature := common.FromHex(rsv)
		if len(signature) != crypto.SignatureLength {
			return "", nil, errWrongSignatureLength
		}
		r := common.ToHex(signature[:32])
		err = mongodb.AddUsedRValue(signPubkey, r)
		if err != nil {
			return "", nil, errRValueIsUsed
		}
	}
	return keyID, rsvs, nil
}

// GetSignStatusByKeyID get sign status by keyID
func GetSignStatusByKeyID(keyID string) (rsvs []string, err error) {
	return getSignResult(keyID, defaultMPCNode.mpcRPCAddress)
}

func getSignResult(keyID, rpcAddr string) (rsvs []string, err error) {
	log.Info("start get sign status", "keyID", keyID)
	var signStatus *SignStatus
	i := 0
	signTimer := time.NewTimer(mpcSignTimeout)
	defer signTimer.Stop()
LOOP_GET_SIGN_STATUS:
	for {
		i++
		select {
		case <-signTimer.C:
			if err == nil {
				err = errSignTimerTimeout
			}
			break LOOP_GET_SIGN_STATUS
		default:
			signStatus, err = GetSignStatus(keyID, rpcAddr)
			if err == nil {
				rsvs = signStatus.Rsv
				break LOOP_GET_SIGN_STATUS
			}
			switch {
			case errors.Is(err, ErrGetSignStatusFailed),
				errors.Is(err, ErrGetSignStatusTimeout):
				break LOOP_GET_SIGN_STATUS
			}
		}
		time.Sleep(3 * time.Second)
	}
	if len(rsvs) == 0 || err != nil {
		log.Info("get sign status failed", "keyID", keyID, "retryCount", i, "err", err)
		return nil, errGetSignResultFailed
	}
	log.Info("get sign status success", "keyID", keyID, "retryCount", i)
	return rsvs, nil
}

// BuildMPCRawTx build mpc raw tx
func BuildMPCRawTx(nonce uint64, payload []byte, keyWrapper *keystore.Key) (string, error) {
	tx := types.NewTransaction(
		nonce,             // nonce
		mpcToAddr,         // to address
		big.NewInt(0),     // value
		100000,            // gasLimit
		big.NewInt(80000), // gasPrice
		payload,           // data
	)
	signature, err := crypto.Sign(mpcSigner.Hash(tx).Bytes(), keyWrapper.PrivateKey)
	if err != nil {
		return "", err
	}
	sigTx, err := tx.WithSignature(mpcSigner, signature)
	if err != nil {
		return "", err
	}
	txdata, err := rlp.EncodeToBytes(sigTx)
	if err != nil {
		return "", err
	}
	rawTX := common.ToHex(txdata)
	return rawTX, nil
}
