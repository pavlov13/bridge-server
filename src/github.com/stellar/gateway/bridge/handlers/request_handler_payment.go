package handlers

import (
	"encoding/hex"
	"io/ioutil"
	"encoding/json"
	log "github.com/Sirupsen/logrus"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	h "github.com/stellar/gateway/horizon"
	"github.com/stellar/gateway/protocols/compliance"
	"github.com/stellar/gateway/protocols/federation"
	"github.com/stellar/gateway/server"
	b "github.com/stellar/go-stellar-base/build"
	"github.com/stellar/go-stellar-base/keypair"
	"github.com/stellar/go-stellar-base/xdr"
)

func (rh *RequestHandler) Payment(w http.ResponseWriter, r *http.Request) {
	source := r.PostFormValue("source")
	sourceKeypair, err := keypair.Parse(source)
	if err != nil {
		log.WithFields(log.Fields{"source": source}).Print("Invalid source parameter")
		server.Write(w, h.NewErrorResponse(h.PaymentInvalidSource))
		return
	}

	destination := r.PostFormValue("destination")
	amount := r.PostFormValue("amount")
	assetCode := r.PostFormValue("asset_code")
	assetIssuer := r.PostFormValue("asset_issuer")
	memoType := r.PostFormValue("memo_type")
	memo := r.PostFormValue("memo")
	extraMemo := r.PostFormValue("extra_memo")

	if extraMemo != "" && rh.Config.Compliance != nil {
		// Compliance server part
		sender := r.PostFormValue("sender")

		resp, err := http.PostForm(
			*rh.Config.Compliance+"/send",
			url.Values{
				"source":       {sourceKeypair.Address()},
				"sender":       {sender},
				"destination":  {destination},
				"amount":       {amount},
				"asset_code":   {assetCode},
				"asset_issuer": {assetIssuer},
				"extra_memo":   {extraMemo},
			},
		)
		if err != nil {
			log.WithFields(log.Fields{"err": err}).Error("Error sending request to compliance server")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Error("Error reading compliance server response")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		if resp.StatusCode != 200 {
			log.WithFields(log.Fields{
				"status": resp.StatusCode,
				"body":   string(body),
			}).Error("Error response from compliance server")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		var complianceSendResponse compliance.SendResponse
		err = json.Unmarshal(body, &complianceSendResponse)
		if err != nil {
			log.Error("Error unmarshalling from compliance server")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		var tx xdr.Transaction
		err = xdr.SafeUnmarshalBase64(complianceSendResponse.TransactionXdr, &tx)
		if err != nil {
			log.Error("Error unmarshalling transaction returned by compliance server")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		submitResponse, err := rh.TransactionSubmitter.SignAndSubmitRawTransaction(*rh.Config.Accounts.IssuingSeed, &tx)
		if err != nil {
			log.WithFields(log.Fields{"err": err}).Error("Error submitting transaction")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		server.Write(w, &submitResponse)
	} else {
		// Payment without compliance server
		destinationObject, _, err := federation.Resolve(destination)
		if err != nil {
			log.WithFields(log.Fields{"destination": destination}).Print("Cannot resolve address")
			server.Write(w, h.NewErrorResponse(h.PaymentCannotResolveDestination))
			return
		}

		_, err = keypair.Parse(destinationObject.AccountId)
		if err != nil {
			log.WithFields(log.Fields{"AccountId": destinationObject.AccountId}).Print("Invalid AccountId in destination")
			server.Write(w, h.NewErrorResponse(h.PaymentInvalidDestination))
			return
		}

		var operationBuilder interface{}

		if assetCode != "" && assetIssuer != "" {
			issuerKeypair, err := keypair.Parse(assetIssuer)
			if err != nil {
				log.WithFields(log.Fields{"asset_issuer": assetIssuer}).Print("Invalid asset_issuer parameter")
				server.Write(w, h.NewErrorResponse(h.PaymentInvalidIssuer))
				return
			}

			operationBuilder = b.Payment(
				b.Destination{destinationObject.AccountId},
				b.CreditAmount{assetCode, issuerKeypair.Address(), amount},
			)
		} else if assetCode == "" && assetIssuer == "" {
			mutators := []interface{}{
				b.Destination{destinationObject.AccountId},
				b.NativeAmount{amount},
			}

			// Check if destination account exist
			_, err = rh.Horizon.LoadAccount(destinationObject.AccountId)
			if err != nil {
				log.WithFields(log.Fields{"error": err}).Error("Error loading account")
				operationBuilder = b.CreateAccount(mutators...)
			} else {
				operationBuilder = b.Payment(mutators...)
			}
		} else {
			log.Print("Missing asset param.")
			server.Write(w, h.NewErrorResponse(h.PaymentMissingParamAsset))
			return
		}

		if !(((memoType == "") && (memo == "")) || ((memoType != "") && (memo != ""))) {
			log.Print("Missing one of memo params.")
			server.Write(w, h.NewErrorResponse(h.PaymentMissingParamMemo))
			return
		}

		if destinationObject.MemoType != nil {
			if memoType != "" {
				log.Print("Memo given in request but federation returned memo fields.")
				server.Write(w, h.NewErrorResponse(h.PaymentCannotUseMemo))
				return
			}

			memoType = *destinationObject.MemoType
			memo = *destinationObject.Memo
		}

		var memoMutator interface{}
		switch {
		case memoType == "":
			break
		case memoType == "id":
			id, err := strconv.ParseUint(memo, 10, 64)
			if err != nil {
				log.WithFields(log.Fields{"memo": memo}).Print("Cannot convert memo_id value to uint64")
				server.Write(w, h.NewErrorResponse(h.PaymentInvalidMemo))
				return
			}
			memoMutator = b.MemoID{id}
		case memoType == "text":
			memoMutator = &b.MemoText{memo}
		case memoType == "hash":
			memoBytes, err := hex.DecodeString(memo)
			if err != nil || len(memoBytes) != 32 {
				log.WithFields(log.Fields{"memo": memo}).Print("Cannot decode hash memo value")
				server.Write(w, h.NewErrorResponse(h.PaymentInvalidMemo))
				return
			}
			var b32 [32]byte
			copy(b32[:], memoBytes[0:32])
			hash := xdr.Hash(b32)
			memoMutator = &b.MemoHash{hash}
		default:
			log.Print("Not supported memo type: ", memoType)
			server.Write(w, h.NewErrorResponse(h.PaymentInvalidMemo))
			return
		}

		accountResponse, err := rh.Horizon.LoadAccount(sourceKeypair.Address())
		if err != nil {
			log.WithFields(log.Fields{"error": err}).Error("Cannot load source account")
			server.Write(w, h.NewErrorResponse(h.PaymentSourceNotExist))
			return
		}

		sequenceNumber, err := strconv.ParseUint(accountResponse.SequenceNumber, 10, 64)
		if err != nil {
			log.WithFields(log.Fields{"error": err}).Error("Cannot convert SequenceNumber")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		transactionMutators := []b.TransactionMutator{
			b.SourceAccount{source},
			b.Sequence{sequenceNumber + 1},
			b.Network{rh.Config.NetworkPassphrase},
			operationBuilder.(b.TransactionMutator),
		}

		if memoMutator != nil {
			transactionMutators = append(transactionMutators, memoMutator.(b.TransactionMutator))
		}

		tx := b.Transaction(transactionMutators...)

		if tx.Err != nil {
			log.WithFields(log.Fields{"err": tx.Err}).Print("Transaction builder error")
			// TODO when build.OperationBuilder interface is ready check for
			// create_account and payment errors separately
			switch {
			case tx.Err.Error() == "Asset code length is invalid":
				server.Write(w, h.NewErrorResponse(h.PaymentMalformedAssetCode))
			case strings.Contains(tx.Err.Error(), "cannot parse amount"):
				server.Write(w, h.NewErrorResponse(h.PaymentInvalidAmount))
			default:
				log.WithFields(log.Fields{"err": tx.Err}).Print("Transaction builder error")
				server.Write(w, h.NewErrorResponse(h.ServerError))
			}
			return
		}

		txe := tx.Sign(source)
		txeB64, err := txe.Base64()

		if err != nil {
			log.WithFields(log.Fields{"error": err}).Error("Cannot encode transaction envelope")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		submitResponse, err := rh.Horizon.SubmitTransaction(txeB64)
		if err != nil {
			log.WithFields(log.Fields{"error": err}).Error("Error submitting transaction")
			server.Write(w, h.NewErrorResponse(h.ServerError))
			return
		}

		server.Write(w, &submitResponse)
	}
}