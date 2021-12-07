package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	oasisTesting "github.com/oasisprotocol/oasis-sdk/client-sdk/go/testing"
	"github.com/stretchr/testify/require"

	"github.com/starfishlabs/oasis-evm-web3-gateway/tests"
)

// Dave's private key for signing Ethereum transactions derived from the seed "oasis-runtime-sdk/test-keys: dave".
var daveKey, _ = crypto.HexToECDSA("c0e43d8755f201b715fd5a9ce0034c568442543ae0a0ee1aec2985ffe40edb99")

func createRequest(method string, params interface{}) Request {
	return Request{
		Version: "2.0",
		Method:  method,
		Params:  params,
		ID:      1,
	}
}

func call(t *testing.T, method string, params interface{}) *Response {
	rawReq, err := json.Marshal(createRequest(method, params))
	require.NoError(t, err)

	time.Sleep(1 * time.Second)
	endpoint, err := w3.GetHTTPEndpoint()
	if err != nil {
		log.Fatalf("failed to obtain HTTP endpoint: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewBuffer(rawReq))
	req.Header.Set("Content-Type", "application/json")
	require.NoError(t, err)

	client := http.Client{}
	res, err := client.Do(req)
	require.NoError(t, err)

	decoder := json.NewDecoder(res.Body)
	rpcRes := new(Response)
	err = decoder.Decode(&rpcRes)
	require.NoError(t, err)

	err = res.Body.Close()
	require.NoError(t, err)
	require.Nil(t, rpcRes.Error)

	return rpcRes
}

func submitTransaction(ctx context.Context, t *testing.T, to common.Address, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) *types.Receipt {
	ec := localClient()
	chainID, err := ec.ChainID(context.Background())
	require.NoError(t, err)

	nonce, err := ec.NonceAt(context.Background(), oasisTesting.Dave.EthAddress, nil)
	require.Nil(t, err, "get nonce failed")

	// Create transaction
	tx := types.NewTransaction(
		nonce,
		to,
		amount,
		gasLimit,
		gasPrice,
		data,
	)
	signer := types.LatestSignerForChainID(chainID)
	signature, err := crypto.Sign(signer.Hash(tx).Bytes(), daveKey)
	require.Nil(t, err, "sign tx")

	signedTx, err := tx.WithSignature(signer, signature)
	require.Nil(t, err, "pack tx")

	err = ec.SendTransaction(context.Background(), signedTx)
	require.Nil(t, err, "send transaction failed")

	receipt, err := waitTransaction(ctx, ec, signedTx.Hash())
	require.NoError(t, err)

	return receipt
}

// Submits a test transaction used in various tests.
func submitTestTransaction(ctx context.Context, t *testing.T) *types.Receipt {
	data := common.FromHex("0x7f7465737432000000000000000000000000000000000000000000000000000000600057")
	to := common.BytesToAddress(common.FromHex("0x1122334455667788990011223344556677889900"))
	return submitTransaction(ctx, t, to, big.NewInt(1), 3000003, big.NewInt(2), data)
}

func TestEth_GetBalance(t *testing.T) {
	ec := localClient()
	res, err := ec.BalanceAt(context.Background(), oasisTesting.Dave.EthAddress, nil)
	require.NoError(t, err)

	t.Logf("Got balance %s for %x\n", res.String(), oasisTesting.Dave.EthAddress)

	require.Greater(t, res.Uint64(), big.NewInt(0).Uint64())
}

func getNonce(t *testing.T, from string) hexutil.Uint64 {
	param := []interface{}{from, "latest"}
	rpcRes := call(t, "eth_getTransactionCount", param)

	var nonce hexutil.Uint64
	err := json.Unmarshal(rpcRes.Result, &nonce)
	require.NoError(t, err)
	return nonce
}

func TestEth_GetTransactionCount(t *testing.T) {
	getNonce(t, fmt.Sprintf("0x%x", oasisTesting.Dave.EthAddress))
}

func localClient() *ethclient.Client {
	url, err := w3.GetHTTPEndpoint()
	if err != nil {
		return nil
	}

	c, _ := ethclient.Dial(url)
	return c
}

func TestEth_ChainID(t *testing.T) {
	ec := localClient()

	id, err := ec.ChainID(context.Background())
	require.Nil(t, err, "get chainid")

	t.Logf("chain id: %v", id)
	require.Equal(t, big.NewInt(int64(tests.TestsConfig.Gateway.ChainID)), id)
}

func TestEth_GasPrice(t *testing.T) {
	ec := localClient()

	price, err := ec.SuggestGasPrice(context.Background())
	require.Nil(t, err, "get gasPrice")

	t.Logf("gas price: %v", price)
}

// TestEth_SendRawTransaction post eth raw transaction with ethclient from go-ethereum.
func TestEth_SendRawTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()

	receipt := submitTransaction(ctx, t, common.Address{1}, big.NewInt(1), 22000, big.NewInt(2), nil)
	require.EqualValues(t, 0, receipt.Status)
}

func TestEth_GetBlockByNumberAndGetBlockByHash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()
	ec := localClient()

	number := big.NewInt(1)
	blk1, err := ec.BlockByNumber(ctx, number)
	require.NoError(t, err)
	require.Equal(t, number, blk1.Number())

	// go-ethereum's Block struct always computes block hash on-the-fly
	// instead of simply returning the hash from BlockBy* API responses.
	// Computing it this way will not work in Oasis because of other non-ethereum
	// transactions in the block which need to be considered, but are not
	// accessible by go-ethereum. To overcome this, we perform getBlockByNumber
	// query with raw HTTP client and use the block's hash from that response.
	// For details, see https://github.com/starfishlabs/oasis-evm-web3-gateway/issues/72
	param := []interface{}{fmt.Sprintf("0x%x", number), false}
	rpcRes := call(t, "eth_getBlockByNumber", param)
	blk2 := make(map[string]interface{})
	err = json.Unmarshal(rpcRes.Result, &blk2)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("0x%x", number), blk2["number"])

	blk3, err := ec.BlockByHash(ctx, common.HexToHash(blk2["hash"].(string)))
	require.NoError(t, err)
	require.Equal(t, number, blk3.Number())
}

func TestEth_GetBlockByNumberLatest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()
	ec := localClient()

	// Explicitly query latest block number.
	block, err := ec.BlockByNumber(ctx, nil)
	require.NoError(t, err, "get latest block number")
	require.Greater(t, block.NumberU64(), uint64(0))
}

func TestEth_GetBlockTransactionCountByNumberLatest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()
	ec := localClient()

	// Explicitly query latest block number.
	_, err := ec.PendingTransactionCount(ctx)
	require.NoError(t, err, "get pending(=latest) transaction count")
}

func TestEth_BlockNumber(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()
	ec := localClient()

	ret, err := ec.BlockNumber(ctx)
	require.NoError(t, err)
	t.Logf("The current block number is %v", ret)
}

func TestEth_GetTransactionByHash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()

	ec := localClient()

	// Submit test transaction.
	input := "0x7f7465737432000000000000000000000000000000000000000000000000000000600057"
	data := common.FromHex(input)
	to := common.BytesToAddress(common.FromHex("0x1122334455667788990011223344556677889900"))
	receipt := submitTransaction(ctx, t, to, big.NewInt(1), 3000003, big.NewInt(2), data)
	require.EqualValues(t, 1, receipt.Status)
	require.NotNil(t, receipt)

	tx2, _, err := ec.TransactionByHash(ctx, receipt.TxHash)
	require.NoError(t, err)
	require.NotNil(t, tx2)
	// Ensure returned transaction hash equals the internally computed one by geth.
	require.Equal(t, tx2.Hash(), receipt.TxHash)

	// Ensure `input` field in response is correctly encoded.
	rsp := make(map[string]interface{})
	rawRsp := call(t, "eth_getTransactionByHash", []string{receipt.TxHash.Hex()})
	require.NoError(t, json.Unmarshal(rawRsp.Result, &rsp))
	require.Equal(t, input, rsp["input"], "getTransactionByHash 'input' response should be correct")
}

func TestEth_GetBlockByHashRawResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()

	// Submit test transaction.
	receipt := submitTestTransaction(ctx, t)
	require.EqualValues(t, 1, receipt.Status)
	require.NotNil(t, receipt)

	// GetBlockByHash(fullTx=false).
	rsp := make(map[string]interface{})
	rawRsp := call(t, "eth_getBlockByHash", []interface{}{receipt.BlockHash.Hex(), false})
	require.NoError(t, json.Unmarshal(rawRsp.Result, &rsp))

	transactions := rsp["transactions"].([]interface{})
	// There should be one transaction in response.
	require.EqualValues(t, 1, len(transactions))
	// The transaction should be a hash.
	require.IsType(t, "string", transactions[0], "getBlockByHash(fullTx=false) should only return transaction hashes")

	// GetBlockByHash(fullTx=true).
	rawRsp = call(t, "eth_getBlockByHash", []interface{}{receipt.BlockHash.Hex(), true})
	require.NoError(t, json.Unmarshal(rawRsp.Result, &rsp))

	transactions = rsp["transactions"].([]interface{})
	// There should be one transaction in response.
	require.EqualValues(t, 1, len(transactions))
	// The transaction should be an object.
	require.IsType(t, make(map[string]interface{}), transactions[0], "getBlockByHash(fullTx=true) should only return full transaction objects")

	// The transaction in getBlockByHash should match transaction obtained by getTransactionByHash.
	txRsp := make(map[string]interface{})
	rawRsp = call(t, "eth_getTransactionByHash", []string{receipt.TxHash.Hex()})
	require.NoError(t, json.Unmarshal(rawRsp.Result, &txRsp))
	require.EqualValues(t, transactions[0], txRsp, "getBlockByHash.transaction should match getTransactionByHash response")
}

func TestEth_GetTransactionReceiptRawResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()

	// Submit test transaction.
	receipt := submitTestTransaction(ctx, t)
	require.Equal(t, uint64(1), receipt.Status)
	require.NotNil(t, receipt)

	// GetTransactionReceipt.
	rsp := make(map[string]interface{})
	rawRsp := call(t, "eth_getTransactionReceipt", []interface{}{receipt.TxHash.Hex()})
	require.NoError(t, json.Unmarshal(rawRsp.Result, &rsp))

	require.Nil(t, rsp["contractAddress"], "contract address should be nil")
}

func TestEth_GetLogsWithoutBlockhash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()

	ec := localClient()
	_, err := ec.FilterLogs(ctx, ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(10)})
	require.NoError(t, err, "getLogs without explicit block hash")
}

func TestEth_GetLogsMultiple(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), OasisBlockTimeout)
	defer cancel()
	ec := localClient()

	code := common.FromHex(strings.TrimSpace(evmEventsTestCompiledHex))

	chainID, err := ec.ChainID(context.Background())
	require.NoError(t, err, "get chainid")

	nonce, err := ec.NonceAt(context.Background(), oasisTesting.Dave.EthAddress, nil)
	require.NoError(t, err, "get nonce failed")

	// Create transaction
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		Value:    big.NewInt(0),
		Gas:      1000000,
		GasPrice: big.NewInt(2),
		Data:     code,
	})
	signer := types.LatestSignerForChainID(chainID)
	signature, err := crypto.Sign(signer.Hash(tx).Bytes(), daveKey)
	require.NoError(t, err, "sign tx")

	signedTx, err := tx.WithSignature(signer, signature)
	require.NoError(t, err, "pack tx")

	err = ec.SendTransaction(context.Background(), signedTx)
	require.NoError(t, err, "send transaction failed")

	receipt, err := waitTransaction(ctx, ec, signedTx.Hash())
	require.NoError(t, err)

	t.Logf("Contract address: %s", receipt.ContractAddress)

	// Ensure successful contract deploy.
	require.Equal(t, uint64(1), receipt.Status)

	// Check emitted logs.
	require.Len(t, receipt.Logs, 3, "3 logs expected")
}
