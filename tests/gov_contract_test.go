// Copyright 2022 The klaytn Authors
// This file is part of the klaytn library.
//
// The klaytn library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The klaytn library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the klaytn library. If not, see <http://www.gnu.org/licenses/>.

package tests

import (
	"encoding/hex"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/klaytn/klaytn/accounts/abi"
	"github.com/klaytn/klaytn/blockchain"
	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/common"
	govcontract "github.com/klaytn/klaytn/contracts/gov"
	"github.com/klaytn/klaytn/crypto"
	"github.com/klaytn/klaytn/governance"
	"github.com/klaytn/klaytn/log"
	"github.com/klaytn/klaytn/node/cn"
	"github.com/klaytn/klaytn/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGovernance_ContractEngine(t *testing.T) {
	log.EnableLogForTest(log.LvlCrit, log.LvlWarn)

	fullNode, node, validator, chainId, workspace := newBlockchain(t)
	defer os.RemoveAll(workspace)
	defer fullNode.Stop()

	var (
		chain        = node.BlockChain().(*blockchain.BlockChain)
		config       = chain.Config()
		owner        = validator
		contractAddr = crypto.CreateAddress(owner.Addr, owner.Nonce)

		paramName  = "istanbul.committeesize"
		paramValue = uint64(22)
		paramBytes = []byte{22}

		deployBlock   uint64 // Before deploy: 0, After deploy: the deployed block
		setParamBlock uint64 // Before setParam: 0, After setParam: the setParam'd block
	)

	// Here we are running (tx sender) and (param reader) in parallel.
	// This is to check that param reader works under various situations such as
	// (a) contract is not yet deployed (b) parameter is not yet set (c) parameter is set.

	// Run tx sender thread
	go func() {
		num, _, _ := deployGovParamTx_constructor(t, node, owner, chainId)
		deployBlock = num

		num, _ = deployGovParamTx_setParamIn(t, node, owner, chainId, contractAddr, paramName, paramBytes)
		setParamBlock = num
	}()

	// Run param reader thread
	config.Governance.GovParamContract = contractAddr
	config.KoreCompatibleBlock = big.NewInt(0)
	engine := governance.NewContractEngine(config)
	engine.SetBlockchain(chain)

	// Validate current params from engine.Params(), alongside block processing.
	// At block #N, Params() returns the parameters to be used when building
	// block #N+1 (i.e. pending block).
	chainEventCh := make(chan blockchain.ChainEvent)
	subscription := chain.SubscribeChainEvent(chainEventCh)
	defer subscription.Unsubscribe()

	for {
		ev := <-chainEventCh
		time.Sleep(100 * time.Millisecond) // wait for tx sender thread to set deployBlock, etc.

		num := ev.Block.Number().Uint64()
		err := engine.UpdateParams()
		assert.Nil(t, err)

		value, _ := engine.Params().Get(params.CommitteeSize)
		t.Logf("Params() at block=%2d: %v", num, value)

		if deployBlock == 0 { // not yet deployed
			assert.Equal(t, nil, value)
		} else if setParamBlock == 0 { // not yet activated
			assert.Equal(t, nil, value)
		} else if num > setParamBlock { // after activation (setParamBlock+1)
			assert.Equal(t, paramValue, value)
		}

		if setParamBlock != 0 && num >= setParamBlock+2 {
			break
		}

		if num >= 60 {
			t.Fatal("test taking too long; something must be wrong")
		}
	}

	// Validate historic params from engine.ParamsAt(n)
	for num := uint64(0); num <= setParamBlock+2; num++ {
		pset, err := engine.ParamsAt(num)
		assert.Nil(t, err)
		assert.NotNil(t, pset)

		value, _ := pset.Get(params.CommitteeSize)
		t.Logf("ParamsAt(block=%2d): %v", num, value)
		if num < deployBlock { // not yet deployed
			assert.Equal(t, nil, value)
		} else if num <= setParamBlock { // not yet activated
			assert.Equal(t, nil, value)
		} else { // after activation
			assert.Equal(t, paramValue, value)
		}
	}
}

func deployGovParamTx_constructor(t *testing.T, node *cn.CN, owner *TestAccountType, chainId *big.Int,
) (uint64, common.Address, *types.Transaction) {
	var (
		// Deploy contract: constructor(address _owner)
		contractAbi, _ = abi.JSON(strings.NewReader(govcontract.GovParamABI))
		contractBin    = govcontract.GovParamBin
		ctorArgs, _    = contractAbi.Pack("")
		code           = contractBin + hex.EncodeToString(ctorArgs)
	)

	// Deploy contract
	tx, addr := deployContractDeployTx(t, node.TxPool(), chainId, owner, code)

	chain := node.BlockChain().(*blockchain.BlockChain)
	receipt := waitReceipt(chain, tx.Hash())
	require.NotNil(t, receipt)
	require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)

	_, _, num, _ := chain.GetTxAndLookupInfo(tx.Hash())
	t.Logf("GovParam deployed at block=%2d, addr=%s", num, addr.Hex())

	return num, addr, tx
}

func deployGovParamTx_setParamIn(t *testing.T, node *cn.CN, owner *TestAccountType, chainId *big.Int,
	contractAddr common.Address, name string, value []byte,
) (uint64, *types.Transaction) {
	var (
		// Add parameter: setParamIn(string name, bytes value)
		contractAbi, _ = abi.JSON(strings.NewReader(govcontract.GovParamABI))
		callArgs, _    = contractAbi.Pack("setParamIn", name, true, value, big.NewInt(1))
		data           = common.ToHex(callArgs)
	)

	tx := deployContractExecutionTx(t, node.TxPool(), chainId, owner, contractAddr, data)

	chain := node.BlockChain().(*blockchain.BlockChain)
	receipt := waitReceipt(chain, tx.Hash())
	require.NotNil(t, receipt)
	require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)

	_, _, num, _ := chain.GetTxAndLookupInfo(tx.Hash())
	t.Logf("GovParam.setParamIn executed at block=%2d", num)
	return num, tx
}

func deployGovParamTx_batchAddParam(t *testing.T, node *cn.CN, owner *TestAccountType, chainId *big.Int,
	contractAddr common.Address, bytesMap map[string][]byte,
) []*types.Transaction {
	var (
		chain          = node.BlockChain().(*blockchain.BlockChain)
		beginBlock     = chain.CurrentHeader().Number.Uint64()
		contractAbi, _ = abi.JSON(strings.NewReader(govcontract.GovParamABI))
		txs            = []*types.Transaction{}
	)

	// Send all setParamIn() calls at once
	for name, value := range bytesMap {
		// Add parameter: setParamIn(string name, bytes value)
		callArgs, _ := contractAbi.Pack("setParamIn", name, true, value, big.NewInt(1))
		data := common.ToHex(callArgs)
		tx := deployContractExecutionTx(t, node.TxPool(), chainId, owner, contractAddr, data)
		txs = append(txs, tx)
	}

	// Wait for all txs
	for _, tx := range txs {
		receipt := waitReceipt(chain, tx.Hash())
		require.NotNil(t, receipt)
		require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
	}
	num := chain.CurrentHeader().Number.Uint64()
	t.Logf("GovParam.setParamIn executed %d times between blocks=%2d,%2d", len(txs), beginBlock, num)
	return txs
}

// Klaytn node only decodes the byte-array param values (refer to params/governance_paramset.go).
// Encoding is the job of transaction senders (i.e. clients and dApps).
// This is a reference implementation of such encoder.
func chainConfigToBytesMap(t *testing.T, config *params.ChainConfig) map[string][]byte {
	pset, err := params.NewGovParamSetChainConfig(config)
	require.Nil(t, err)
	strMap := pset.StrMap()

	bytesMap := map[string][]byte{}
	for name, value := range strMap {
		switch value.(type) {
		case string:
			bytesMap[name] = []byte(value.(string))
		case common.Address:
			bytesMap[name] = value.(common.Address).Bytes()
		case uint64:
			bytesMap[name] = new(big.Int).SetUint64(value.(uint64)).Bytes()
		case bool:
			if value.(bool) == true {
				bytesMap[name] = []byte{0x01}
			} else {
				bytesMap[name] = []byte{0x00}
			}
		}
	}

	// Check that bytesMap is correct just in case
	qset, err := params.NewGovParamSetBytesMap(bytesMap)
	require.Nil(t, err)
	require.Equal(t, pset.StrMap(), qset.StrMap())
	return bytesMap
}
