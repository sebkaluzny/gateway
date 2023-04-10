package servers

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bloXroute-Labs/gateway/v2"
	"github.com/bloXroute-Labs/gateway/v2/blockchain"
	"github.com/bloXroute-Labs/gateway/v2/bxmessage"
	"github.com/bloXroute-Labs/gateway/v2/connections"
	"github.com/bloXroute-Labs/gateway/v2/jsonrpc"
	log "github.com/bloXroute-Labs/gateway/v2/logger"
	"github.com/bloXroute-Labs/gateway/v2/sdnmessage"
	"github.com/bloXroute-Labs/gateway/v2/types"
	"github.com/bloXroute-Labs/gateway/v2/utils"
	"github.com/bloXroute-Labs/gateway/v2/utils/orderedmap"
	"github.com/bloXroute-Labs/gateway/v2/utils/syncmap"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/gorilla/websocket"
	uuid "github.com/satori/go.uuid"
	"github.com/sourcegraph/jsonrpc2"
	websocketjsonrpc2 "github.com/sourcegraph/jsonrpc2/websocket"
	"github.com/zhouzhuojie/conditions"
	"golang.org/x/sync/errgroup"
)

// ClientHandler is a struct for gateway client handler object
type ClientHandler struct {
	feedManager     *FeedManager
	websocketServer *http.Server
	httpServer      *HTTPServer
	log             *log.Entry
	getQuotaUsage   func(accountID string) (*connections.QuotaResponseBody, error)
}

// MultiTransactions - response for MultiTransactions subscription
type MultiTransactions struct {
	Subscription string     `json:"subscription"`
	Result       []TxResult `json:"result"`
}

// TxResponse - response of the jsonrpc params
type TxResponse struct {
	Subscription string   `json:"subscription"`
	Result       TxResult `json:"result"`
}

// TxResult - request of jsonrpc params
type TxResult struct {
	TxHash      *string     `json:"txHash,omitempty"`
	TxContents  interface{} `json:"txContents,omitempty"`
	LocalRegion *bool       `json:"localRegion,omitempty"`
	Time        *string     `json:"time,omitempty"`
	RawTx       *string     `json:"rawTx,omitempty"`
}

// TxResultWithEthTx - request of jsonrpc params with an eth type transaction
type TxResultWithEthTx struct {
	TxHash      *string               `json:"txHash,omitempty"`
	TxContents  *ethtypes.Transaction `json:"txContents,omitempty"`
	LocalRegion *bool                 `json:"localRegion,omitempty"`
	Time        *string               `json:"time,omitempty"`
	RawTx       *string               `json:"rawTx,omitempty"`
}

// BlockResponse - response of the jsonrpc params
type BlockResponse struct {
	Subscription string             `json:"subscription"`
	Result       types.Notification `json:"result"`
}

type handlerObj struct {
	FeedManager       *FeedManager
	ClientReq         *clientReq
	remoteAddress     string
	connectionAccount sdnmessage.Account
	log               *log.Entry
	getQuotaUsage     func(accountID string) (*connections.QuotaResponseBody, error)
}

type clientReq struct {
	includes []string
	feed     types.FeedType
	expr     conditions.Expr
	calls    *map[string]*RPCCall
	MultiTxs bool
}

type subscriptionRequest struct {
	feed    types.FeedType
	options subscriptionOptions
}

// subscriptionOptions includes subscription options
type subscriptionOptions struct {
	Include    []string            `json:"Include"`
	Filters    string              `json:"Filters"`
	CallParams []map[string]string `json:"Call-Params"`
	MultiTxs   bool                `json:"MultiTxs"`
}

// RPCCall represents customer call executed for onBlock feed
type RPCCall struct {
	commandMethod string
	blockOffset   int
	callName      string
	callPayload   map[string]string
	active        bool
}

var (
	// ErrWSConnDelay amount of time to sleep before closing a bad connection. This is configured by tests to a shorted value
	ErrWSConnDelay = 10 * time.Second
)

func newCall(name string) *RPCCall {
	return &RPCCall{
		callName:    name,
		callPayload: make(map[string]string),
		active:      true,
	}
}

// NewClientHandler is a constructor for ClientHandler
func NewClientHandler(feedManager *FeedManager, websocketServer *http.Server, httpServer *HTTPServer, log *log.Entry, getQuotaUsage func(accountID string) (*connections.QuotaResponseBody, error)) ClientHandler {
	return ClientHandler{
		feedManager:     feedManager,
		websocketServer: websocketServer,
		httpServer:      httpServer,
		log:             log,
		getQuotaUsage:   getQuotaUsage,
	}
}

func (c *RPCCall) validatePayload(method string, requiredFields []string) error {
	for _, field := range requiredFields {
		_, ok := c.callPayload[field]
		if !ok {
			return fmt.Errorf("expected %v element in request payload for %v", field, method)
		}
	}
	return nil
}

func (c *RPCCall) string() string {
	payloadBytes, err := json.Marshal(c.callPayload)
	if err != nil {
		log.Errorf("failed to convert eth call to string: %v", err)
		return c.callName
	}

	return fmt.Sprintf("%+v", struct {
		commandMethod string
		blockOffset   int
		callName      string
		callPayload   string
		active        bool
	}{
		commandMethod: c.commandMethod,
		blockOffset:   c.blockOffset,
		callName:      c.callName,
		callPayload:   string(payloadBytes),
		active:        c.active,
	})
}

type rpcPingResponse struct {
	Pong string `json:"pong"`
}

type rpcTxResponse struct {
	TxHash string `json:"txHash"`
}

type rpcBatchTxResponse struct {
	TxHashes []string `json:"txHashes"`
}

var upgrader = websocket.Upgrader{}

var txContentFields = []string{"tx_contents.nonce", "tx_contents.tx_hash",
	"tx_contents.gas_price", "tx_contents.gas", "tx_contents.to", "tx_contents.value", "tx_contents.input",
	"tx_contents.v", "tx_contents.r", "tx_contents.s", "tx_contents.from", "tx_contents.type", "tx_contents.access_list",
	"tx_contents.chain_id", "tx_contents.max_priority_fee_per_gas", "tx_contents.max_fee_per_gas"}

var validTxParams = append(txContentFields, "tx_contents", "tx_hash", "local_region", "time", "raw_tx")

var validBlockParams = append(txContentFields, "hash", "header", "transactions", "uncles", "future_validator_info")

var validOnBlockParams = []string{"name", "response", "block_height", "tag"}

var validBeaconBlockParams = []string{"hash", "header", "slot", "body"}

var validTxReceiptParams = []string{"block_hash", "block_number", "contract_address",
	"cumulative_gas_used", "effective_gas_price", "from", "gas_used", "logs", "logs_bloom",
	"status", "to", "transaction_hash", "transaction_index", "type"}

var validParams = map[types.FeedType][]string{
	types.NewTxsFeed:     validTxParams,
	types.BDNBlocksFeed:  validBlockParams,
	types.NewBlocksFeed:  validBlockParams,
	types.PendingTxsFeed: validTxParams,
	types.OnBlockFeed:    validOnBlockParams,
	types.TxReceiptsFeed: validTxReceiptParams,

	// Beacon
	types.NewBeaconBlocksFeed: validBeaconBlockParams,
	types.BDNBeaconBlocksFeed: validBeaconBlockParams,
}

var defaultTxParams = append(txContentFields, "tx_hash", "local_region", "time")

var availableFilters = []string{"gas", "gas_price", "value", "to", "from", "method_id", "type", "chain_id", "max_fee_per_gas", "max_priority_fee_per_gas"}

var operators = []string{"=", ">", "<", "!=", ">=", "<=", "in"}
var operands = []string{"and", "or"}

var availableFeeds = []types.FeedType{types.NewTxsFeed, types.NewBlocksFeed, types.BDNBlocksFeed, types.PendingTxsFeed, types.OnBlockFeed, types.TxReceiptsFeed, types.NewBeaconBlocksFeed, types.BDNBeaconBlocksFeed}

// NewWSServer creates and returns a new websocket server managed by FeedManager
func NewWSServer(feedManager *FeedManager, getQuotaUsage func(accountID string) (*connections.QuotaResponseBody, error)) *http.Server {
	handler := http.NewServeMux()
	wsHandler := func(responseWriter http.ResponseWriter, request *http.Request) {
		connectionAccountID, connectionSecretHash, err := getAccountIDSecretHashFromReq(request, feedManager.cfg.WebsocketTLSEnabled)
		if err != nil {
			log.Errorf("RemoteAddr: %v RequestURI: %v - %v.", request.RemoteAddr, request.RequestURI, err.Error())
			errorWithDelay(responseWriter, request, "failed parsing the authorization header")
			return
		}
		connectionAccountModel := sdnmessage.Account{}
		serverAccountID := feedManager.accountModel.AccountID
		// if gateway received request from a customer with a different account id, it should verify it with the SDN.
		//if the gateway does not have permission to verify account id (which mostly happen with external gateways),
		//SDN will return StatusUnauthorized and fail this connection. if SDN return any other error -
		//assuming the issue is with the SDN and set default enterprise account for the customer. in order to send request to the gateway,
		//customer must be enterprise / elite account
		if connectionAccountID != serverAccountID {
			connectionAccountModel, err = feedManager.getCustomerAccountModel(connectionAccountID)
			if err != nil {
				if strings.Contains(strconv.FormatInt(http.StatusUnauthorized, 10), err.Error()) {
					log.Errorf("Account %v is not authorized to get other account %v information", serverAccountID, connectionAccountID)
					errorWithDelay(responseWriter, request, "account is not authorized to get other accounts information")
					return
				}
				log.Errorf("Failed to get customer account model, account id: %v, remote addr: %v, connectionSecretHash: %v, error: %v",
					connectionAccountID, request.RemoteAddr, connectionSecretHash, err)
				connectionAccountModel = sdnmessage.GetDefaultEliteAccount(time.Now().UTC())
				connectionAccountModel.AccountID = connectionAccountID
				connectionAccountModel.SecretHash = connectionSecretHash
			}
			if !connectionAccountModel.TierName.IsEnterprise() {
				log.Warnf("Customer account %v must be enterprise / enterprise elite / ultra but it is %v", connectionAccountID, connectionAccountModel.TierName)
				errorWithDelay(responseWriter, request, "account must be enterprise / enterprise elite / ultra")
				return
			}
		} else {
			connectionAccountModel = feedManager.accountModel
		}
		if connectionAccountModel.SecretHash != connectionSecretHash && connectionSecretHash != "" {
			log.Errorf("Account %v sent a different secret hash than set in the account model, remoteAddress: %v", connectionAccountID, request.RemoteAddr)
			errorWithDelay(responseWriter, request, "wrong value in the authorization header")
			return
		}
		handleWSClientConnection(feedManager, responseWriter, request, connectionAccountModel, getQuotaUsage)
	}

	handler.HandleFunc("/ws", wsHandler)
	handler.HandleFunc("/", wsHandler)

	server := http.Server{
		Addr:    fmt.Sprintf(":%v", feedManager.cfg.WebsocketPort),
		Handler: handler,
	}
	return &server
}

func errorWithDelay(w http.ResponseWriter, r *http.Request, msg string) {
	// sleep for 10 seconds to prevent the client (bot) to reissue the same requests in a loop
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("error replying with delay to request from RemoteAddr %v - %v", r.RemoteAddr, err.Error())
		time.Sleep(ErrWSConnDelay)
		return
	}
	_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, msg))
	time.Sleep(ErrWSConnDelay)
	c.Close()
}

// handleWsClientConnection - when new http connection is made we get here upgrade to ws, and start handling
func handleWSClientConnection(feedManager *FeedManager, w http.ResponseWriter, r *http.Request, accountModel sdnmessage.Account, getQuotaUsage func(accountID string) (*connections.QuotaResponseBody, error)) {
	log.Debugf("New web-socket connection from %v", r.RemoteAddr)
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("error upgrading HTTP server connection to the WebSocket protocol - %v", err.Error())
		http.Error(w, "error upgrading HTTP server connection to the WebSocket protocol", http.StatusUpgradeRequired)
		time.Sleep(ErrWSConnDelay)
		return
	}

	logger := log.WithFields(log.Fields{
		"component":  "handlerObj",
		"remoteAddr": r.RemoteAddr,
	})

	handler := &handlerObj{
		FeedManager:       feedManager,
		remoteAddress:     r.RemoteAddr,
		connectionAccount: accountModel,
		log:               logger,
		getQuotaUsage:     getQuotaUsage,
	}

	asynHhandler := jsonrpc2.AsyncHandler(handler)
	_ = jsonrpc2.NewConn(r.Context(), websocketjsonrpc2.NewObjectStream(connection), asynHhandler)
}

func getAccountIDSecretHashFromReq(request *http.Request, websocketTLSEnabled bool) (accountID types.AccountID, secretHash string, err error) {
	tokenString := request.Header.Get("Authorization")
	var payload []byte
	if tokenString != "" {
		payload, err = base64.StdEncoding.DecodeString(tokenString)
		if err != nil {
			err = fmt.Errorf("DecodeString error: %v invalid Authorization: %v", err, tokenString)
			return
		}
		accountIDAndHash := strings.SplitN(string(payload), ":", 2)
		if len(accountIDAndHash) == 1 {
			err = fmt.Errorf("invalid Authorization: %v palyoad: %v, authorization can be generate from account_id", tokenString, payload)
			return
		}
		accountID = types.AccountID(accountIDAndHash[0])
		secretHash = accountIDAndHash[1]
	} else if websocketTLSEnabled {
		if request.TLS != nil && len(request.TLS.PeerCertificates) > 0 {
			accountID, err = utils.GetAccountIDFromBxCertificate(request.TLS.PeerCertificates[0].Extensions)
			if err != nil {
				err = fmt.Errorf("failed to get account_id extension, %w", err)
				return
			}
		}
	}
	if accountID == "" {
		err = fmt.Errorf("missing authorization from method: %v", request.Method)
		return
	}
	return
}

func (ch *ClientHandler) runWSServer() {
	ch.websocketServer = NewWSServer(ch.feedManager, ch.getQuotaUsage)
	ch.log.Infof("starting websockets RPC server at: %v", ch.websocketServer.Addr)
	var err error
	if ch.feedManager.cfg.WebsocketTLSEnabled {
		ch.websocketServer.TLSConfig = &tls.Config{
			ClientAuth: tls.RequestClientCert,
		}
		err = ch.websocketServer.ListenAndServeTLS(ch.feedManager.certFile, ch.feedManager.keyFile)
	} else {
		err = ch.websocketServer.ListenAndServe()
	}
	if err != nil {
		ch.log.Warnf("websockets RPC server is closed: %v", err)
	}
}

func (ch *ClientHandler) shutdownWSServer() {
	ch.log.Infof("shutting down websocket server")
	ch.feedManager.CloseAllClientConnections()
	err := ch.websocketServer.Shutdown(ch.feedManager.context)
	if err != nil {
		ch.log.Errorf("encountered error shutting down websocket server %v: %v", ch.feedManager.cfg.WebsocketPort, err)
	}
}

// ManageWSServer manage the ws connection of the blockchain node
func (ch *ClientHandler) ManageWSServer(activeManagement bool) {
	if activeManagement {
		for {
			select {
			case syncStatus := <-ch.feedManager.nodeWSManager.ReceiveNodeSyncStatusUpdate():
				if syncStatus == blockchain.Unsynced {
					ch.shutdownWSServer()
					ch.feedManager.subscriptionServices.SendSubscriptionResetNotification(make([]sdnmessage.SubscriptionModel, 0))
				} else {
					go ch.runWSServer()
				}
			}
		}
	} else {
		go ch.runWSServer()
		for {
			select {
			case <-ch.feedManager.nodeWSManager.ReceiveNodeSyncStatusUpdate():
				// consume update
			}
		}
	}
}

// ManageHTTPServer runs http server for the gateway client handler
func (ch *ClientHandler) ManageHTTPServer(ctx context.Context) {
	ch.httpServer.Start()
	<-ctx.Done()
	ch.httpServer.Stop()
}

// SendErrorMsg formats and sends an RPC error message back to the client
func SendErrorMsg(ctx context.Context, code jsonrpc.RPCErrorCode, data string, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	rpcError := &jsonrpc2.Error{
		Code:    int64(code),
		Message: jsonrpc.ErrorMsg[code],
	}
	rpcError.SetError(data)
	err := conn.ReplyWithError(ctx, req.ID, rpcError)
	if err != nil {
		// TODO: move this to caller and add identifying information
		log.Errorf("could not respond to client with error message: %v", err)
	}
}

func reply(ctx context.Context, conn *jsonrpc2.Conn, ID jsonrpc2.ID, result interface{}) error {
	if err := conn.Reply(ctx, ID, result); err != nil {
		return err
	}
	return nil
}

func (h *handlerObj) validateFeed(feedName types.FeedType, feedStreaming sdnmessage.BDNFeedService, includes []string, filters []string) error {
	expireDateTime, _ := time.Parse(bxgateway.TimeDateLayoutISO, feedStreaming.ExpireDate)
	if time.Now().UTC().After(expireDateTime) {
		return fmt.Errorf("%v is not allowed or date has been expired", feedName)
	}
	if feedStreaming.Feed.AllowFiltering && utils.Exists("all", feedStreaming.Feed.AvailableFields) {
		return nil
	}
	for _, include := range includes {
		if !utils.Exists(include, feedStreaming.Feed.AvailableFields) {
			return fmt.Errorf("including %v: %v is not allowed", feedName, include)
		}
	}
	if !feedStreaming.Feed.AllowFiltering && len(filters) > 0 {
		return fmt.Errorf("filtering in %v is not allowed", feedName)
	}
	return nil
}

func (h *handlerObj) filterAndInclude(clientReq *clientReq, tx *types.NewTransactionNotification) *TxResult {
	hasTxContent := false
	if clientReq.expr != nil {
		txFilters := tx.Filters(clientReq.expr.Args())
		if txFilters == nil {
			return nil
		}
		//Evaluate if we should send the tx
		shouldSend, err := conditions.Evaluate(clientReq.expr, txFilters)
		if err != nil {
			h.log.Errorf("error evaluate Filters. feed: %v. method: %v. Filters: %v. remote address: %v. account id: %v error - %v tx: %v.",
				clientReq.feed, clientReq.includes[0], clientReq.expr.String(), h.remoteAddress, h.connectionAccount.AccountID, err.Error(), txFilters)
			return nil
		}
		if !shouldSend {
			return nil
		}
	}
	var response TxResult
	for _, param := range clientReq.includes {
		if strings.Contains(param, "tx_contents") {
			hasTxContent = true
		}
		switch param {
		case "tx_hash":
			txHash := tx.GetHash()
			response.TxHash = &txHash
		case "time":
			timeNow := time.Now().Format(bxgateway.MicroSecTimeFormat)
			response.Time = &timeNow
		case "local_region":
			localRegion := tx.LocalRegion()
			response.LocalRegion = &localRegion
		case "raw_tx":
			rawTx := hexutil.Encode(tx.RawTx())
			response.RawTx = &rawTx
		}
	}
	if hasTxContent {
		fields := tx.Fields(clientReq.includes)
		if fields == nil {
			log.Errorf("Got nil from tx.Fields - need to be checked")
			return nil
		}
		response.TxContents = fields
	}
	return &response
}

func (h *handlerObj) subscribeMultiTxs(ctx context.Context, feedChan *chan *types.Notification, subscriptionID *uuid.UUID, clientReq *clientReq, conn *jsonrpc2.Conn, req *jsonrpc2.Request, feedName types.FeedType) error {
	for {
		select {
		case <-conn.DisconnectNotify():
			return nil
		case notification, ok := <-(*feedChan):
			continueProcessing := true
			multiTxsResponse := MultiTransactions{Subscription: subscriptionID.String()}
			if !ok {
				if h.FeedManager.SubscriptionExists(*subscriptionID) {
					SendErrorMsg(ctx, jsonrpc.InternalError, string(rune(websocket.CloseMessage)), conn, req)
				}
				return errors.New("error when reading new notification")
			}
			switch feedName {
			case types.NewTxsFeed:
				tx := (*notification).(*types.NewTransactionNotification)
				response := h.filterAndInclude(clientReq, tx)
				if response != nil {
					multiTxsResponse.Result = append(multiTxsResponse.Result, *response)
				}
			case types.PendingTxsFeed:
				tx := (*notification).(*types.PendingTransactionNotification)
				response := h.filterAndInclude(clientReq, &tx.NewTransactionNotification)
				if response != nil {
					multiTxsResponse.Result = append(multiTxsResponse.Result, *response)
				}
			}
			for continueProcessing {
				select {
				case <-conn.DisconnectNotify():
					return nil
				case notification, ok := <-(*feedChan):
					if !ok {
						if h.FeedManager.SubscriptionExists(*subscriptionID) {
							SendErrorMsg(ctx, jsonrpc.InternalError, string(rune(websocket.CloseMessage)), conn, req)
						}
						return errors.New("error when reading new notification")
					}
					switch feedName {
					case types.NewTxsFeed:
						tx := (*notification).(*types.NewTransactionNotification)
						response := h.filterAndInclude(clientReq, tx)
						if response != nil {
							multiTxsResponse.Result = append(multiTxsResponse.Result, *response)
						}
					case types.PendingTxsFeed:
						tx := (*notification).(*types.PendingTransactionNotification)
						response := h.filterAndInclude(clientReq, &tx.NewTransactionNotification)
						if response != nil {
							multiTxsResponse.Result = append(multiTxsResponse.Result, *response)
						}
					}
					if len(multiTxsResponse.Result) >= 50 {
						continueProcessing = false
					}
				default:
					continueProcessing = false
				}
			}
			if len(multiTxsResponse.Result) > 0 {
				err := conn.Notify(ctx, "subscribe", multiTxsResponse)
				if err != nil {
					h.log.Errorf("error notify to subscriptionID: %v : %v ", subscriptionID, err.Error())
					return err
				}
			}
		}
	}
}

// Handle - handling client request
func (h *handlerObj) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	start := time.Now()
	defer func() {
		h.log.Debugf("websocket handling for method %v ended. Duration %v", jsonrpc.RPCRequestType(req.Method), time.Since(start))
	}()
	switch jsonrpc.RPCRequestType(req.Method) {
	case jsonrpc.RPCSubscribe:
		request, err := h.createClientReq(req)
		if err != nil {
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}
		feedName := request.feed
		if len(h.FeedManager.nodeWSManager.Providers()) == 0 {
			switch request.feed {
			case types.NewTxsFeed:
			case types.BDNBlocksFeed:
			case types.NewBeaconBlocksFeed:
			case types.BDNBeaconBlocksFeed:
			case types.NewBlocksFeed:
				// Blocks in consensus come not from websocket
				if h.FeedManager.networkNum == bxgateway.RopstenNum || h.FeedManager.networkNum == bxgateway.GoerliNum || h.FeedManager.networkNum == bxgateway.MainnetNum {
					break
				}

				fallthrough
			default:
				errMsg := fmt.Sprintf("%v feed requires a websockets endpoint to be specifed via either --eth-ws-uri or --multi-node startup parameter", feedName)
				SendErrorMsg(ctx, jsonrpc.InvalidParams, errMsg, conn, req)
				return
			}
		}
		var filters string
		if request.expr != nil {
			filters = request.expr.String()
		}
		sub, errSubscribe := h.FeedManager.Subscribe(request.feed, conn, h.connectionAccount.TierName, h.connectionAccount.AccountID, h.remoteAddress, filters, strings.Join(request.includes, ","), "")

		if errSubscribe != nil {
			SendErrorMsg(ctx, jsonrpc.InvalidParams, errSubscribe.Error(), conn, req)
			return
		}
		subscriptionID := sub.SubscriptionID

		defer h.FeedManager.Unsubscribe(*subscriptionID, false, "")
		if err = reply(ctx, conn, req.ID, subscriptionID); err != nil {
			h.log.Errorf("error reply to %v with subscriptionID: %v : %v ", h.remoteAddress, subscriptionID, err)
			SendErrorMsg(ctx, jsonrpc.InternalError, string(rune(websocket.CloseMessage)), conn, req)
			return
		}
		h.FeedManager.stats.LogSubscribeStats(subscriptionID,
			h.connectionAccount.AccountID,
			feedName,
			h.connectionAccount.TierName,
			h.remoteAddress,
			h.FeedManager.networkNum,
			request.includes,
			filters,
			"")

		if request.MultiTxs {
			if feedName != types.NewTxsFeed && feedName != types.PendingTxsFeed {
				SendErrorMsg(ctx, jsonrpc.InvalidParams, "multi tx support only in new txs or pending txs", conn, req)
				log.Debugf("multi tx support only in new txs or pending txs, subscription id %v, account id %v, remote addr %v", subscriptionID.String(), h.connectionAccount.AccountID, h.remoteAddress)
				return
			}
			err := h.subscribeMultiTxs(ctx, sub.FeedChan, subscriptionID, request, conn, req, feedName)
			if err != nil {
				log.Errorf("error while processing %v (%v) with multi tx argument, %v", feedName, subscriptionID, err)
				return
			}
		}

		for {
			select {
			case <-conn.DisconnectNotify():
				return
			case errMsg := <-sub.ErrMsgChan:
				SendErrorMsg(ctx, jsonrpc.InvalidParams, errMsg, conn, req)
				return
			case notification, ok := <-(*sub.FeedChan):
				if !ok {
					if h.FeedManager.SubscriptionExists(*subscriptionID) {
						SendErrorMsg(ctx, jsonrpc.InternalError, string(rune(websocket.CloseMessage)), conn, req)
					}
					return
				}
				switch feedName {
				case types.NewTxsFeed:
					tx := (*notification).(*types.NewTransactionNotification)
					if h.sendTxNotification(ctx, subscriptionID, request, conn, tx) != nil {
						return
					}
				case types.PendingTxsFeed:
					tx := (*notification).(*types.PendingTransactionNotification)
					if h.sendTxNotification(ctx, subscriptionID, request, conn, &tx.NewTransactionNotification) != nil {
						return
					}
				case types.BDNBlocksFeed, types.NewBlocksFeed, types.NewBeaconBlocksFeed, types.BDNBeaconBlocksFeed:
					if h.sendNotification(ctx, subscriptionID, request, conn, *notification) != nil {
						return
					}
				case types.TxReceiptsFeed:
					block := (*notification).(*types.EthBlockNotification)
					nodeWS, ok := h.getSyncedWSProvider(block.Source())
					if !ok {
						return
					}

					g := new(errgroup.Group)
					for _, t := range block.Transactions {
						tx := t
						g.Go(func() error {
							hash := tx["hash"]
							response, err := nodeWS.FetchTransactionReceipt([]interface{}{hash}, blockchain.RPCOptions{RetryAttempts: bxgateway.MaxEthTxReceiptCallRetries, RetryInterval: bxgateway.EthTxReceiptCallRetrySleepInterval})
							if err != nil || response == nil {
								h.log.Debugf("failed to fetch transaction receipt for %v in block %v: %v", hash, block.BlockHash, err)
								return err
							}
							txReceiptNotification := types.NewTxReceiptNotification(response.(map[string]interface{}))
							if err = h.sendNotification(ctx, subscriptionID, request, conn, txReceiptNotification); err != nil {
								h.log.Errorf("failed to send tx receipt for %v err %v", hash, err)
								return err
							}
							return err
						})
					}
					if err := g.Wait(); err != nil {
						return
					}
					h.log.Debugf("finished fetching transaction receipts for block %v", block.BlockHash)
				case types.OnBlockFeed:
					block := (*notification).(*types.EthBlockNotification)
					nodeWS, ok := h.getSyncedWSProvider(block.Source())
					if !ok {
						return
					}
					blockHeightStr := block.Header.Number
					hashStr := block.BlockHash.String()

					var wg sync.WaitGroup
					for _, c := range *request.calls {
						wg.Add(1)
						go func(call *RPCCall) {
							defer wg.Done()
							if !call.active {
								return
							}
							tag := hexutil.EncodeUint64(block.Header.GetNumber() + uint64(call.blockOffset))
							payload, err := h.FeedManager.nodeWSManager.ConstructRPCCallPayload(call.commandMethod, call.callPayload, tag)
							if err != nil {
								return
							}
							response, err := nodeWS.CallRPC(call.commandMethod, payload, blockchain.RPCOptions{RetryAttempts: bxgateway.MaxEthOnBlockCallRetries, RetryInterval: bxgateway.EthOnBlockCallRetrySleepInterval})
							if err != nil {
								h.log.Debugf("disabling failed onBlock call %v: %v", call.callName, err)
								call.active = false
								taskDisabledNotification := types.NewOnBlockNotification(bxgateway.TaskDisabledEvent, call.string(), blockHeightStr, tag, hashStr)
								if h.sendNotification(ctx, subscriptionID, request, conn, taskDisabledNotification) != nil {
									h.log.Errorf("failed to send TaskDisabledNotification for %v", call.callName)
								}
								return
							}
							onBlockNotification := types.NewOnBlockNotification(call.callName, response.(string), blockHeightStr, tag, hashStr)
							if h.sendNotification(ctx, subscriptionID, request, conn, onBlockNotification) != nil {
								return
							}
						}(c)
					}
					wg.Wait()
					taskCompletedNotification := types.NewOnBlockNotification(bxgateway.TaskCompletedEvent, "", blockHeightStr, blockHeightStr, hashStr)
					if h.sendNotification(ctx, subscriptionID, request, conn, taskCompletedNotification) != nil {
						h.log.Errorf("failed to send TaskCompletedEvent on block %v", blockHeightStr)
						return
					}
				}
			}
		}
	case jsonrpc.RPCUnsubscribe:
		var params []string
		if req.Params == nil {
			err := fmt.Errorf("params is missing in the request")
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}
		_ = json.Unmarshal(*req.Params, &params)
		if len(params) != 1 {
			err := fmt.Errorf("params %v with incorrect length", params)
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}
		uid, _ := uuid.FromString(params[0])
		if err := h.FeedManager.Unsubscribe(uid, false, ""); err != nil {
			h.log.Infof("subscription id %v was not found", uid)
			if err := reply(ctx, conn, req.ID, "false"); err != nil {
				h.log.Errorf("error reply to %v on unsubscription on subscriptionID: %v : %v ", h.remoteAddress, uid, err)
				SendErrorMsg(ctx, jsonrpc.InternalError, string(rune(websocket.CloseMessage)), conn, req)
				return
			}
		}
		if err := reply(ctx, conn, req.ID, "true"); err != nil {
			h.log.Errorf("error reply to %v on unsubscription on subscriptionID: %v : %v ", h.remoteAddress, uid, err)
			SendErrorMsg(ctx, jsonrpc.InternalError, string(rune(websocket.CloseMessage)), conn, req)
			return
		}
	case jsonrpc.RPCTx:
		if h.FeedManager.accountModel.AccountID != h.connectionAccount.AccountID {
			err := fmt.Errorf("blxr_tx is not allowed when account authentication is different from the node account")
			if h.FeedManager.accountModel.AccountID == types.BloxrouteAccountID {
				h.log.Infof("received a tx from user account %v, remoteAddr %v, %v", h.connectionAccount.AccountID, h.remoteAddress, err)
			} else {
				h.log.Errorf("%v. account auth: %v, node account: %v ", err, h.connectionAccount.AccountID, h.FeedManager.accountModel.AccountID)
				SendErrorMsg(ctx, jsonrpc.InvalidRequest, err.Error(), conn, req)
			}
			return
		}
		var params jsonrpc.RPCTxPayload
		err := json.Unmarshal(*req.Params, &params)
		if err != nil {
			h.log.Errorf("unmarshal req.Params error - %v", err.Error())
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		// if user tried to send transaction directly to the internal gateway, return error
		if h.FeedManager.accountModel.AccountID == types.BloxrouteAccountID && types.AccountID(params.OriginalSenderAccountID) == types.EmptyAccountID {
			h.log.Errorf("cannot send transaction to internal gateway directly")
			SendErrorMsg(ctx, jsonrpc.InvalidRequest, "failed to send transaction", conn, req)
			return
		}

		var ws connections.RPCConn
		if h.connectionAccount.AccountID == types.BloxrouteAccountID {
			// Tx sent from cloud services, need to update account ID of the connection to be the origin sender
			ws = connections.NewRPCConn(types.AccountID(params.OriginalSenderAccountID), h.remoteAddress, h.FeedManager.networkNum, utils.CloudAPI)
		} else {
			ws = connections.NewRPCConn(h.connectionAccount.AccountID, h.remoteAddress, h.FeedManager.networkNum, utils.Websocket)
		}

		txHash, ok := h.handleSingleTransaction(ctx, conn, req, params.Transaction, ws, params.ValidatorsOnly, true, params.NextValidator, params.Fallback, h.FeedManager.nextValidatorMap, h.FeedManager.validatorStatusMap, params.NodeValidation, params.FrontRunningProtection)
		if !ok {
			return
		}

		response := rpcTxResponse{
			TxHash: txHash,
		}

		if err = reply(ctx, conn, req.ID, response); err != nil {
			h.log.Errorf("%v reply error - %v", jsonrpc.RPCTx, err)
			return
		}
		h.log.Infof("blxr_tx: Hash - 0x%v", response.TxHash)
	case jsonrpc.RPCBatchTx:
		var txHashes []string
		if h.FeedManager.accountModel.AccountID != h.connectionAccount.AccountID {
			err := fmt.Errorf("blxr_batch_tx is not allowed when account authentication is different from the node account")
			h.log.Errorf("%v. account auth: %v, node account: %v ", err, h.connectionAccount.AccountID, h.FeedManager.accountModel.AccountID)
			return
		}
		var params jsonrpc.RPCBatchTxPayload
		err := json.Unmarshal(*req.Params, &params)
		if err != nil {
			h.log.Errorf("unmarshal req.Params error - %v", err.Error())
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		var ws connections.RPCConn
		if h.connectionAccount.AccountID == types.BloxrouteAccountID {
			// Tx sent from cloud services, need to update account ID of the connection to be the origin sender
			ws = connections.NewRPCConn(types.AccountID(params.OriginalSenderAccountID), h.remoteAddress, h.FeedManager.networkNum, utils.CloudAPI)
		} else {
			ws = connections.NewRPCConn(h.connectionAccount.AccountID, h.remoteAddress, h.FeedManager.networkNum, utils.Websocket)
		}

		for _, transaction := range params.Transactions {
			txHash, ok := h.handleSingleTransaction(ctx, conn, req, transaction, ws, params.ValidatorsOnly, false, false, 0, nil, nil, false, false)
			if !ok {
				continue
			}
			txHashes = append(txHashes, txHash)
		}

		if len(txHashes) == 0 {
			err = fmt.Errorf("all transactions are invalid")
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		response := rpcBatchTxResponse{
			TxHashes: txHashes,
		}

		if err = reply(ctx, conn, req.ID, response); err != nil {
			h.log.Errorf("%v reply error - %v", jsonrpc.RPCBatchTx, err)
			return
		}
		h.log.Infof("blxr_batch_tx: Hashes - %v", response.TxHashes)
	case jsonrpc.RPCPing:
		response := rpcPingResponse{
			Pong: time.Now().UTC().Format(bxgateway.MicroSecTimeFormat),
		}
		if err := reply(ctx, conn, req.ID, response); err != nil {
			h.log.Errorf("%v reply error - %v", jsonrpc.RPCPing, err)
		}
	case jsonrpc.RPCMEVSearcher:
		if h.FeedManager.accountModel.AccountID != h.connectionAccount.AccountID {
			err := fmt.Errorf("blxr_mev_searcher is not allowed when account authentication is different from the node account")
			h.log.Errorf("%v. account auth: %v, node account: %v ", err, h.connectionAccount.AccountID, h.FeedManager.accountModel.AccountID)
			SendErrorMsg(ctx, jsonrpc.AccountIDError, err.Error(), conn, req)
			return
		}
		params := struct {
			MEVMethod         string            `json:"mev_method"`
			Payload           json.RawMessage   `json:"payload"`
			MEVBuilders       map[string]string `json:"mev_builders"`
			Frontrunning      bool              `json:"frontrunning"`
			EffectiveGasPrice big.Int           `json:"effective_gas_price"`
			CoinbaseProfit    big.Int           `json:"coinbase_profit"`
		}{}

		if req.Params == nil {
			err := errors.New("failed to unmarshal req.Params for mevSearcher, params not found")
			h.log.Error(err)
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		err := json.Unmarshal(*req.Params, &params)
		if err != nil {
			h.log.Errorf("failed to unmarshal req.Params for mevSearcher, error: %v", err.Error())
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		sendBundleArgs := []sendBundleArgs{}
		err = json.Unmarshal(params.Payload, &sendBundleArgs)
		if err != nil {
			h.log.Errorf("failed to unmarshal req.Params for mevSearcher, error: %v", err.Error())
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		if len(sendBundleArgs) != 1 {
			h.log.Errorf("received invalid number of mevSearcher payload, must be 1 element")
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		if err := sendBundleArgs[0].validate(); err != nil {
			h.log.Errorf("mevSearcher payload validation failed: %v", err)
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return

		}

		for mevBuilder := range params.MEVBuilders {
			if strings.ToLower(mevBuilder) == bxgateway.BloxrouteBuilderName && !h.connectionAccount.TierName.IsElite() {
				h.log.Warnf("EnterpriseElite account is required in order to send %s to %s", jsonrpc.RPCMEVSearcher, bxgateway.BloxrouteBuilderName)
			}
		}
		mevSearcher, err := bxmessage.NewMEVSearcher(
			params.MEVMethod,
			params.MEVBuilders,
			sendBundleArgs[0].UUID,
			params.Frontrunning,
			params.EffectiveGasPrice,
			params.CoinbaseProfit,
			params.Payload,
		)
		if err != nil {
			h.log.Errorf("failed to create new mevSearcher: %v", err)
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		ws := connections.NewRPCConn(h.connectionAccount.AccountID, h.remoteAddress, h.FeedManager.networkNum, utils.Websocket)

		mevSearcher.SetHash()
		mevSearcher.SetNetworkNum(h.FeedManager.networkNum)
		err = h.FeedManager.node.HandleMsg(&mevSearcher, ws, connections.RunForeground)
		if err != nil {
			h.log.Errorf("failed to process mevSearcher message: %v", err)
			SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
			return
		}

		if err := reply(ctx, conn, req.ID, map[string]string{"status": "ok"}); err != nil {
			h.log.Errorf("%v mev searcher error: %v", jsonrpc.RPCMEVSearcher, err)
		}
	case jsonrpc.RPCQuotaUsage:
		accountID := fmt.Sprint(h.connectionAccount.AccountID)
		quotaRes, err := h.getQuotaUsage(accountID)
		if err != nil {
			sendErr := fmt.Errorf("failed to fetch quota usage: %v", err)
			SendErrorMsg(ctx, jsonrpc.MethodNotFound, sendErr.Error(), conn, req)
		}
		if err = reply(ctx, conn, req.ID, quotaRes); err != nil {
			h.log.Errorf("%v reply error - %v", jsonrpc.RPCQuotaUsage, err)
		}

	default:
		err := fmt.Errorf("got unsupported method name: %v", req.Method)
		SendErrorMsg(ctx, jsonrpc.MethodNotFound, err.Error(), conn, req)
		return
	}
}

func (h *handlerObj) getSyncedWSProvider(preferredProviderEndpoint *types.NodeEndpoint) (blockchain.WSProvider, bool) {
	if !h.FeedManager.nodeWSManager.Synced() {
		return nil, false
	}
	nodeWS, ok := h.FeedManager.nodeWSManager.Provider(preferredProviderEndpoint)
	if !ok || nodeWS.SyncStatus() != blockchain.Synced {
		nodeWS, ok = h.FeedManager.nodeWSManager.SyncedProvider()
	}
	return nodeWS, ok
}

// sendNotification - build a response according to client request and notify client
func (h *handlerObj) sendNotification(ctx context.Context, subscriptionID *uuid.UUID, clientReq *clientReq, conn *jsonrpc2.Conn, notification types.Notification) error {
	response := BlockResponse{
		Subscription: subscriptionID.String(),
	}
	content := notification.WithFields(clientReq.includes)
	response.Result = content
	err := conn.Notify(ctx, "subscribe", response)
	if err != nil {
		h.log.Errorf("error reply to subscriptionID: %v : %v ", subscriptionID, err.Error())
		return err
	}
	return nil
}

// sendTxNotification - build a response according to client request and notify client
func (h *handlerObj) sendTxNotification(ctx context.Context, subscriptionID *uuid.UUID, clientReq *clientReq, conn *jsonrpc2.Conn, tx *types.NewTransactionNotification) error {
	result := h.filterAndInclude(clientReq, tx)
	if result == nil {
		return nil
	}
	response := TxResponse{
		Subscription: subscriptionID.String(),
		Result:       *result,
	}

	err := conn.Notify(ctx, "subscribe", response)
	if err != nil {
		h.log.Errorf("error notify to subscriptionID: %v : %v ", subscriptionID, err.Error())
		return err
	}

	return nil
}

func (h *handlerObj) createClientReq(req *jsonrpc2.Request) (*clientReq, error) {
	if req.Params == nil {
		return nil, fmt.Errorf("invalid json request: params is a required field")
	}
	request := subscriptionRequest{}
	var rpcParams []json.RawMessage
	err := json.Unmarshal(*req.Params, &rpcParams)
	if err != nil {
		return nil, err
	}
	if len(rpcParams) < 2 {
		h.log.Debugf("invalid param from request id: %v. method: %v. params: %s. remote address: %v account id: %v.",
			req.ID, req.Method, *req.Params, h.remoteAddress, h.connectionAccount.AccountID)
		return nil, fmt.Errorf("number of params must be at least length 2. requested params: %s", *req.Params)
	}
	err = json.Unmarshal(rpcParams[0], &request.feed)
	if err != nil {
		return nil, err
	}
	if !types.Exists(request.feed, availableFeeds) {
		h.log.Debugf("invalid param from request id: %v. method: %v. params: %s. remote address: %v account id: %v",
			req.ID, req.Method, *req.Params, h.remoteAddress, h.connectionAccount.AccountID)
		return nil, fmt.Errorf("got unsupported feed name %v. possible feeds are %v", request.feed, availableFeeds)
	}
	if h.connectionAccount.AccountID != h.FeedManager.accountModel.AccountID &&
		(request.feed == types.OnBlockFeed || request.feed == types.TxReceiptsFeed) {
		err = fmt.Errorf("%v feed is not available via cloud services. %v feed is only supported on gateways", request.feed, request.feed)
		h.log.Errorf("%v. caller account ID: %v, node account ID: %v ", err, h.connectionAccount.AccountID, h.FeedManager.accountModel.AccountID)
		return nil, err
	}

	err = json.Unmarshal(rpcParams[1], &request.options)
	if err != nil {
		return nil, err
	}
	if request.options.Include == nil {
		h.log.Debugf("invalid param from request id: %v. method: %v. params: %s. remote address: %v account id: %v.",
			req.ID, req.Method, *req.Params, h.remoteAddress, h.connectionAccount.AccountID)
		return nil, fmt.Errorf("got unsupported params %v", string(rpcParams[1]))
	}
	var requestedFields []string
	if len(request.options.Include) == 0 {
		switch request.feed {
		case types.BDNBlocksFeed, types.NewBlocksFeed, types.BDNBeaconBlocksFeed, types.NewBeaconBlocksFeed:
			requestedFields = validBlockParams
		case types.NewTxsFeed:
			requestedFields = defaultTxParams
		case types.PendingTxsFeed:
			requestedFields = defaultTxParams
		case types.OnBlockFeed:
			requestedFields = validParams[types.OnBlockFeed]
		case types.TxReceiptsFeed:
			requestedFields = validParams[types.TxReceiptsFeed]
		}
	}
	for _, param := range request.options.Include {
		switch request.feed {
		case types.BDNBlocksFeed:
			if !utils.Exists(param, validParams[types.BDNBlocksFeed]) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		case types.NewBlocksFeed:
			if !utils.Exists(param, validParams[types.NewBlocksFeed]) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		case types.BDNBeaconBlocksFeed:
			if !utils.Exists(param, validParams[types.BDNBeaconBlocksFeed]) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		case types.NewBeaconBlocksFeed:
			if !utils.Exists(param, validParams[types.NewBeaconBlocksFeed]) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		case types.NewTxsFeed:
			if !utils.Exists(param, validTxParams) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		case types.PendingTxsFeed:
			if !utils.Exists(param, validTxParams) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		case types.OnBlockFeed:
			if !utils.Exists(param, validParams[types.OnBlockFeed]) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		case types.TxReceiptsFeed:
			if !utils.Exists(param, validParams[types.TxReceiptsFeed]) {
				return nil, fmt.Errorf("got unsupported param %v", param)
			}
		}
		if param == "tx_contents" {
			requestedFields = append(requestedFields, txContentFields...)
		}
		requestedFields = append(requestedFields, param)
	}
	request.options.Include = requestedFields

	var expr conditions.Expr
	if request.options.Filters != "" {
		// Parse the condition language and get expression
		_, expr, err = ParseFilter(request.options.Filters)
		if err != nil {
			h.log.Debugf("error parsing Filters from request id: %v. method: %v. params: %s. remote address: %v account id: %v error - %v",
				req.ID, req.Method, *req.Params, h.remoteAddress, h.connectionAccount.AccountID, err.Error())
			return nil, fmt.Errorf("error parsing Filters %v", err.Error())

		}
		err = EvaluateFilters(expr)
		if err != nil {
			h.log.Debugf("error evalued Filters from request id: %v. method: %v. params: %s. remote address: %v account id: %v error - %v",
				req.ID, req.Method, *req.Params, h.remoteAddress, h.connectionAccount.AccountID, err.Error())
			return nil, fmt.Errorf("error evaluat Filters- %v", err.Error())
		}
		if expr != nil {
			h.log.Infof("GetTxContentAndFilters string - %s, GetTxContentAndFilters args - %s", expr, expr.Args())
		}
	}

	// check if valid feed
	var filters []string
	if expr != nil {
		filters = expr.Args()
	}

	feedStreaming := sdnmessage.BDNFeedService{}
	switch request.feed {
	case types.NewTxsFeed:
		feedStreaming = h.connectionAccount.NewTransactionStreaming
	case types.PendingTxsFeed:
		feedStreaming = h.connectionAccount.PendingTransactionStreaming
	case types.BDNBlocksFeed, types.NewBlocksFeed, types.NewBeaconBlocksFeed, types.BDNBeaconBlocksFeed:
		feedStreaming = h.connectionAccount.NewBlockStreaming
	case types.OnBlockFeed:
		feedStreaming = h.connectionAccount.OnBlockFeed
	case types.TxReceiptsFeed:
		feedStreaming = h.connectionAccount.TransactionReceiptFeed
	}
	err = h.validateFeed(request.feed, feedStreaming, request.options.Include, filters)
	if err != nil {
		return nil, err
	}

	calls := make(map[string]*RPCCall)
	if request.feed == types.OnBlockFeed {
		for idx, callParams := range request.options.CallParams {
			if callParams == nil {
				return nil, fmt.Errorf("call-params cannot be nil")
			}
			call := newCall(strconv.Itoa(idx))
			for param, value := range callParams {
				switch param {
				case "method":
					isValidMethod := utils.Exists(value, h.FeedManager.nodeWSManager.ValidRPCCallMethods())
					if !isValidMethod {
						return nil, fmt.Errorf("invalid method %v provided. Supported methods: %v", value, h.FeedManager.nodeWSManager.ValidRPCCallMethods())
					}
					call.commandMethod = value
				case "tag":
					if value == "latest" {
						call.blockOffset = 0
						break
					}
					blockOffset, err := strconv.Atoi(value)
					if err != nil || blockOffset > 0 {
						return nil, fmt.Errorf("invalid value %v provided for tag. Supported values: latest, 0 or a negative number", value)
					}
					call.blockOffset = blockOffset
				case "name":
					_, nameExists := calls[value]
					if nameExists {
						return nil, fmt.Errorf("unique name must be provided for each call: call %v already exists", value)
					}
					call.callName = value
				default:
					isValidPayloadField := utils.Exists(param, h.FeedManager.nodeWSManager.ValidRPCCallPayloadFields())
					if !isValidPayloadField {
						return nil, fmt.Errorf("invalid payload field %v provided. Supported fields: %v", param, h.FeedManager.nodeWSManager.ValidRPCCallPayloadFields())
					}
					call.callPayload[param] = value
				}
			}
			requiredFields, ok := h.FeedManager.nodeWSManager.RequiredPayloadFieldsForRPCMethod(call.commandMethod)
			if !ok {
				return nil, fmt.Errorf("unexpectedly, unable to find required fields for method %v", call.commandMethod)
			}
			err = call.validatePayload(call.commandMethod, requiredFields)
			if err != nil {
				return nil, err
			}
			calls[call.callName] = call
		}
	}

	clientRequest := &clientReq{}
	clientRequest.includes = request.options.Include
	clientRequest.feed = request.feed
	clientRequest.expr = expr
	clientRequest.MultiTxs = request.options.MultiTxs
	clientRequest.calls = &calls
	return clientRequest, nil
}

// ParseFilter parsing the filter
func ParseFilter(filters string) (string, conditions.Expr, error) {
	// if the filters values are go-type filters, for example: {value}, parse the filters
	// if not go-type, convert it to go-type filters
	if strings.Contains(filters, "{") {
		p := conditions.NewParser(strings.NewReader(strings.ToLower(strings.Replace(filters, "'", "\"", -1))))
		expr, err := p.Parse()
		if err == nil {
			isEmptyValue := filtersHasEmptyValue(expr.String())
			if isEmptyValue != nil {
				return "", nil, errors.New("filter is empty")
			}
		}

		return "", expr, err
	}

	// convert the string and add whitespace to separate elements
	tempFilters := strings.ReplaceAll(filters, "(", " ( ")
	tempFilters = strings.ReplaceAll(tempFilters, ")", " ) ")
	tempFilters = strings.ReplaceAll(tempFilters, "[", " [ ")
	tempFilters = strings.ReplaceAll(tempFilters, "]", " ] ")
	tempFilters = strings.ReplaceAll(tempFilters, ",", " , ")
	tempFilters = strings.ReplaceAll(tempFilters, "=", " = ")
	tempFilters = strings.ReplaceAll(tempFilters, "<", " < ")
	tempFilters = strings.ReplaceAll(tempFilters, ">", " > ")
	tempFilters = strings.ReplaceAll(tempFilters, "!", " ! ")
	tempFilters = strings.ReplaceAll(tempFilters, ",", " , ")
	tempFilters = strings.ReplaceAll(tempFilters, "<  =", "<=")
	tempFilters = strings.ReplaceAll(tempFilters, ">  =", ">=")
	tempFilters = strings.ReplaceAll(tempFilters, "!  =", "!=")
	tempFilters = strings.Trim(tempFilters, " ")
	tempFilters = strings.ToLower(tempFilters)
	filtersArr := strings.Split(tempFilters, " ")

	var newFilterString strings.Builder
	for _, elem := range filtersArr {
		switch {
		case elem == "":
		case elem == "(", elem == ")", elem == ",", elem == "]", elem == "[":
			newFilterString.WriteString(elem)
		case utils.Exists(elem, operators):
			newFilterString.WriteString(" ")
			if elem == "=" {
				newFilterString.WriteString(elem)
			}
			newFilterString.WriteString(elem + " ")
		case utils.Exists(elem, operands):
			newFilterString.WriteString(")")
			newFilterString.WriteString(" " + elem + " ")
		case utils.Exists(elem, availableFilters):
			newFilterString.WriteString("({" + elem + "}")
		default:
			isString := false
			if _, err := strconv.Atoi(elem); err != nil {
				isString = true
			}
			switch {
			case isString && len(elem) >= 2 && elem[0:2] != "0x":
				newFilterString.WriteString("'0x" + elem + "'")
			case isString && len(elem) >= 2 && elem[0:2] == "0x":
				newFilterString.WriteString("'" + elem + "'")
			default:
				newFilterString.WriteString(elem)
			}
		}
	}

	newFilterString.WriteString(")")

	p := conditions.NewParser(strings.NewReader(strings.ToLower(strings.Replace(newFilterString.String(), "'", "\"", -1))))
	expr, err := p.Parse()

	if err == nil {
		isEmptyValue := filtersHasEmptyValue(expr.String())
		if isEmptyValue != nil {
			return "", nil, errors.New("filter is empty")
		}
	}

	return newFilterString.String(), expr, err
}

func filtersHasEmptyValue(rawFilters string) error {
	rex := regexp.MustCompile(`\(([^)]+)\)`)
	out := rex.FindAllStringSubmatch(rawFilters, -1)
	for _, i := range out {
		for _, filter := range availableFilters {
			if i[1] == filter || filter == rawFilters {
				return fmt.Errorf("%v", i[1])
			}
		}
	}
	return nil
}

// EvaluateFilters - evaluating if the Filters provided by the user are ok
func EvaluateFilters(expr conditions.Expr) error {
	//Evaluate if we should send the tx
	_, err := conditions.Evaluate(expr, types.EmptyFilteredTransactionMap)
	return err
}

func (h *handlerObj) handleSingleTransaction(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request, transaction string, ws connections.Conn, validatorsOnly bool, sendError bool, nextValidator bool, fallback uint16, nextValidatorMap *orderedmap.OrderedMap, validatorStatusMap *syncmap.SyncMap[string, bool], nodeValidationRequested bool, frontRunningProtection bool) (string, bool) {
	h.FeedManager.LockPendingNextValidatorTxs()

	txContent, err := types.DecodeHex(transaction)
	if err != nil {
		SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
		return "", false
	}
	tx, pendingReevaluation, err := ValidateTxFromExternalSource(transaction, txContent, validatorsOnly, h.FeedManager.chainID, nextValidator, fallback, nextValidatorMap, validatorStatusMap, h.FeedManager.networkNum, ws.GetAccountID(), nodeValidationRequested, h.FeedManager.nodeWSManager, ws, h.FeedManager.pendingBSCNextValidatorTxHashToInfo, frontRunningProtection)
	h.FeedManager.UnlockPendingNextValidatorTxs()
	if err != nil && sendError {
		SendErrorMsg(ctx, jsonrpc.InvalidParams, err.Error(), conn, req)
		return "", false
	}

	if !pendingReevaluation {
		// call the Handler. Don't invoke in a go routine
		err = h.FeedManager.node.HandleMsg(tx, ws, connections.RunForeground)
		if err != nil {
			log.Errorf("failed to handle single transaction: %v", err)
			return "", false
		}
	} else if fallback != 0 {
		// BSC first validator was not accessible and fallback > BSCBlockTime
		// in case fallback time is up before next validator is evaluated, send tx as normal tx at fallback time
		// (tx with fallback less than BSCBlockTime are not marked as pending)
		time.AfterFunc(time.Duration(uint64(fallback)*bxgateway.MillisecondsToNanosecondsMultiplier), func() {
			h.FeedManager.LockPendingNextValidatorTxs()
			defer h.FeedManager.UnlockPendingNextValidatorTxs()
			if _, exists := h.FeedManager.pendingBSCNextValidatorTxHashToInfo[tx.Hash().String()]; exists {
				delete(h.FeedManager.pendingBSCNextValidatorTxHashToInfo, tx.Hash().String())
				log.Infof("sending next validator tx %v because fallback time reached", tx.Hash().String())

				tx.RemoveFlags(types.TFNextValidator)
				tx.SetFallback(0)
				err = h.FeedManager.node.HandleMsg(tx, ws, connections.RunForeground)
				if err != nil {
					log.Errorf("failed to send pending next validator tx %v at fallback time: %v", tx.Hash().String(), err)
				}
			}
		})
	}

	return tx.Hash().String(), true
}

// ValidateTxFromExternalSource validate transaction from external source (ws / grpc), return bool indicates if tx is pending reevaluation
func ValidateTxFromExternalSource(transaction string, txBytes []byte, validatorsOnly bool, gatewayChainID types.NetworkID, nextValidator bool, fallback uint16, nextValidatorMap *orderedmap.OrderedMap, validatorStatusMap *syncmap.SyncMap[string, bool], networkNum types.NetworkNum, accountID types.AccountID, nodeValidationRequested bool, wsManager blockchain.WSManager, source connections.Conn, pendingBSCNextValidatorTxHashToInfo map[string]PendingNextValidatorTxInfo, frontRunningProtection bool) (*bxmessage.Tx, bool, error) {
	// Ethereum's transactions encoding for RPC interfaces is slightly different from the RLP encoded format, so decode + re-encode the transaction for consistency.
	// Specifically, note `UnmarshalBinary` should be used for RPC interfaces, and rlp.DecodeBytes should be used for the wire protocol.
	var ethTx ethtypes.Transaction
	err := ethTx.UnmarshalBinary(txBytes)
	if err != nil {
		// If UnmarshalBinary failed, we will try RLP in case user made mistake
		e := rlp.DecodeBytes(txBytes, &ethTx)
		if e != nil {
			return nil, false, err
		}
		log.Warnf("Ethereum transaction was in RLP format instead of binary," +
			" transaction has been processed anyway, but it'd be best to use the Ethereum binary standard encoding")
	}

	if ethTx.ChainId().Int64() != 0 && gatewayChainID != 0 && types.NetworkID(ethTx.ChainId().Int64()) != gatewayChainID {
		log.Debugf("chainID mismatch for hash %v - tx chainID %v , gateway networkNum %v networkChainID %v", ethTx.Hash().String(), ethTx.ChainId().Int64(), networkNum, gatewayChainID)
		return nil, false, fmt.Errorf("chainID mismatch for hash %v, expect %v got %v, make sure the tx is sent with the right blockchain network", ethTx.Hash().String(), gatewayChainID, ethTx.ChainId().Int64())
	}

	txContent, err := rlp.EncodeToBytes(&ethTx)

	if err != nil {
		return nil, false, err
	}

	var txFlags = types.TFPaidTx | types.TFLocalRegion
	if validatorsOnly {
		txFlags |= types.TFValidatorsOnly
	} else if nextValidator {
		txFlags |= types.TFNextValidator
	} else {
		txFlags |= types.TFDeliverToNode
	}

	if frontRunningProtection {
		txFlags |= types.TFFrontRunningProtection
	}

	var hash types.SHA256Hash
	copy(hash[:], ethTx.Hash().Bytes())

	// should set the account of the sender, not the account of the gateway itself
	tx := bxmessage.NewTx(hash, txContent, networkNum, txFlags, accountID)
	if nextValidator {
		txPendingReevaluation, err := ProcessNextValidatorTx(tx, fallback, nextValidatorMap, validatorStatusMap, networkNum, source, pendingBSCNextValidatorTxHashToInfo)
		if err != nil {
			return nil, false, err
		}
		if txPendingReevaluation {
			return tx, true, nil
		}
	}

	if nodeValidationRequested && !tx.Flags().IsNextValidator() && !tx.Flags().IsValidatorsOnly() {
		syncedWS, ok := wsManager.SyncedProvider()
		if ok {
			_, err := syncedWS.SendTransaction(
				fmt.Sprintf("%v%v", "0x", transaction),
				blockchain.RPCOptions{
					RetryAttempts: 1,
					RetryInterval: 10 * time.Millisecond,
				},
			)
			if err != nil {
				if !strings.Contains(err.Error(), "already known") { // gateway propagates tx to node before doing this check
					errMsg := fmt.Sprintf("tx (%v) failed node validation with error: %v", tx.Hash(), err.Error())
					return nil, false, errors.New(errMsg)
				}
			}
		} else {
			return nil, false, fmt.Errorf("failed to validate tx (%v) via node: no synced WS provider available", tx.Hash())
		}
	}
	return tx, false, nil
}

type sendBundleArgs struct {
	Txs               []hexutil.Bytes `json:"txs"`
	UUID              string          `json:"uuid,omitempty"`
	BlockNumber       string          `json:"blockNumber"`
	MinTimestamp      uint64          `json:"minTimestamp"`
	MaxTimestamp      uint64          `json:"maxTimestamp"`
	RevertingTxHashes []common.Hash   `json:"revertingTxHashes"`
}

// ProcessNextValidatorTx - sets next validator wallets if accessible and returns bool indicating if tx is pending reevaluation due to inaccessible first validator for BSC
func ProcessNextValidatorTx(tx *bxmessage.Tx, fallback uint16, nextValidatorMap *orderedmap.OrderedMap, validatorStatusMap *syncmap.SyncMap[string, bool], networkNum types.NetworkNum, source connections.Conn, pendingBSCNextValidatorTxHashToInfo map[string]PendingNextValidatorTxInfo) (bool, error) {
	if networkNum != bxgateway.BSCMainnetNum && networkNum != bxgateway.PolygonMainnetNum {
		return false, errors.New("currently next_validator is only supported on BSC and Polygon networks, please contact bloXroute support")
	}

	if nextValidatorMap == nil {
		log.Errorf("failed to process next validator tx, because next validator map is nil, tx %v", tx.Hash().String())
		return false, errors.New("failed to send next validator tx, please contact bloXroute support")
	}

	tx.SetFallback(fallback)

	// take the latest two blocks from the ordered map for updating txMsg walletID
	n2Validator := nextValidatorMap.Newest()
	if n2Validator == nil {
		return false, errors.New("can't send tx with next_validator because the gateway encountered an issue fetching the epoch block, please try again later or contact bloXroute support")
	}

	if networkNum == bxgateway.BSCMainnetNum {
		n1Validator := n2Validator.Prev()
		n1ValidatorAccessible := false
		n1Wallet := ""
		if n1Validator != nil {
			n1Wallet = n1Validator.Value.(string)
			accessible, exist := validatorStatusMap.Load(n1Wallet)
			if exist {
				n1ValidatorAccessible = accessible
			}
		}

		if n1ValidatorAccessible {
			tx.SetWalletID(0, n1Wallet)
		} else {
			blockIntervalBSC := bxgateway.NetworkToBlockDuration[bxgateway.BSCMainnet]
			if fallback != 0 && fallback < uint16(blockIntervalBSC.Milliseconds()) {
				return false, nil
			}
			pendingBSCNextValidatorTxHashToInfo[tx.Hash().String()] = PendingNextValidatorTxInfo{
				Tx:            tx,
				Fallback:      fallback,
				TimeOfRequest: time.Now(),
				Source:        source,
			}
			return true, nil
		}
	}

	if networkNum == bxgateway.PolygonMainnetNum {
		n1Validator := n2Validator.Prev()
		if n1Validator != nil {
			tx.SetWalletID(0, n1Validator.Value.(string))
			tx.SetWalletID(1, n2Validator.Value.(string))
		} else {
			tx.SetWalletID(0, n2Validator.Value.(string))
		}
	}

	return false, nil
}

func (s *sendBundleArgs) validate() error {
	if len(s.Txs) == 0 && s.UUID == "" {
		return errors.New("bundle missing txs")
	}

	if s.BlockNumber == "" {
		return errors.New("bundle missing blockNumber")
	}

	for _, encodedTx := range s.Txs {
		tx := new(ethtypes.Transaction)
		if err := tx.UnmarshalBinary(encodedTx); err != nil {
			return err
		}
	}

	if s.UUID != "" {
		_, err := uuid.FromString(s.UUID)
		if err != nil {
			return fmt.Errorf("invalid UUID, %v", err)
		}
	}

	_, err := hexutil.DecodeUint64(s.BlockNumber)
	if err != nil {
		return fmt.Errorf("blockNumber must be hex, %v", err)
	}

	return nil
}
