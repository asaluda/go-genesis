// Copyright 2016 The go-daylight Authors
// This file is part of the go-daylight library.
//
// The go-daylight library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-daylight library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-daylight library. If not, see <http://www.gnu.org/licenses/>.

package parser

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/EGaaS/go-egaas-mvp/packages/config/syspar"
	"github.com/EGaaS/go-egaas-mvp/packages/consts"
	"github.com/EGaaS/go-egaas-mvp/packages/converter"
	"github.com/EGaaS/go-egaas-mvp/packages/crypto"
	"github.com/EGaaS/go-egaas-mvp/packages/model"
	"github.com/EGaaS/go-egaas-mvp/packages/script"
	"github.com/EGaaS/go-egaas-mvp/packages/smart"
	"github.com/EGaaS/go-egaas-mvp/packages/utils"
	"github.com/EGaaS/go-egaas-mvp/packages/utils/tx"

	"github.com/shopspring/decimal"
	log "github.com/sirupsen/logrus"
	"gopkg.in/vmihailenco/msgpack.v2"
)

type Block struct {
	Header     utils.BlockData
	PrevHeader *utils.BlockData
	MrklRoot   []byte
	BinData    []byte
	Parsers    []*Parser
}

func (b Block) GetLogger() *log.Entry {
	return log.WithFields(log.Fields{"block_id": b.Header.BlockID, "block_time": b.Header.Time, "block_wallet_id": b.Header.WalletID,
		"block_state_id": b.Header.StateID, "block_hash": b.Header.Hash, "block_version": b.Header.Version})
}

func InsertBlock(data []byte) error {
	block, err := ProcessBlock(data)
	if err != nil {
		return err
	}

	if err := block.CheckBlock(); err != nil {
		return err
	}

	err = block.PlayBlockSafe()
	if err != nil {
		return err
	}

	log.WithFields(log.Fields{"block_id": block.Header.BlockID}).Debug("block was inserted successfully")
	return nil
}

func (block *Block) PlayBlockSafe() error {
	logger := block.GetLogger()
	dbTransaction, err := model.StartTransaction()
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("starting db transaction")
		return err
	}

	err = block.playBlock(dbTransaction)
	if err != nil {
		dbTransaction.Rollback()
		return err
	}

	if err := UpdBlockInfo(dbTransaction, block); err != nil {
		dbTransaction.Rollback()
		return err
	}

	if err := InsertIntoBlockchain(dbTransaction, block); err != nil {
		dbTransaction.Rollback()
		return err
	}

	dbTransaction.Commit()
	return nil
}

func ProcessBlock(data []byte) (*Block, error) {
	if int64(len(data)) > syspar.GetMaxBlockSize() {
		log.WithFields(log.Fields{"size": len(data), "max_size": syspar.GetMaxBlockSize()}).Error("block size exceeds max block size")
		return nil, utils.ErrInfo(fmt.Errorf(`len(binaryBlock) > variables.Int64["max_block_size"]`))
	}

	buf := bytes.NewBuffer(data)
	if buf.Len() == 0 {
		log.Error("block data is empty")
		return nil, fmt.Errorf("empty buffer")
	}

	block, err := parseBlock(buf)
	if err != nil {
		return nil, err
	}
	block.BinData = data

	if err := block.readPreviousBlock(); err != nil {
		return nil, err
	}

	return block, nil
}

func getAllTables() (map[string]string, error) {
	allTables, err := model.GetAllTables()
	if err != nil {
		log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting all tables")
		return nil, utils.ErrInfo(err)
	}
	AllPkeys := make(map[string]string)
	for _, table := range allTables {
		col, err := model.GetFirstColumnName(table)
		if err != nil {
			log.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting table first column name")
			return nil, utils.ErrInfo(err)
		}
		AllPkeys[table] = col
	}
	return AllPkeys, nil
}

func parseBlock(blockBuffer *bytes.Buffer) (*Block, error) {
	header, err := ParseBlockHeader(blockBuffer)
	if err != nil {
		return nil, err
	}
	logger := log.WithFields(log.Fields{"block_id": header.BlockID, "block_time": header.Time, "block_wallet_id": header.WalletID,
		"block_state_id": header.StateID, "block_hash": header.Hash, "block_version": header.Version})

	allKeys, err := getAllTables()
	if err != nil {
		return nil, err
	}
	parsers := make([]*Parser, 0)

	var mrklSlice [][]byte

	// parse transactions
	for blockBuffer.Len() > 0 {
		transactionSize, err := converter.DecodeLengthBuf(blockBuffer)
		if err != nil {
			logger.WithFields(log.Fields{"type": consts.UnmarshallingError, "error": err}).Error("transaction size is 0")
			return nil, fmt.Errorf("bad block format (%s)", err)
		}
		if blockBuffer.Len() < int(transactionSize) {
			logger.WithFields(log.Fields{"size": blockBuffer.Len(), "match_size": int(transactionSize)}).Error("transaction size does not matches encoded length")
			return nil, fmt.Errorf("bad block format (transaction len is too big: %d)", transactionSize)
		}

		if transactionSize == 0 {
			logger.Error("transaction size is 0")
			return nil, fmt.Errorf("transaction size is 0")
		}

		bufTransaction := bytes.NewBuffer(blockBuffer.Next(int(transactionSize)))
		p, err := ParseTransaction(bufTransaction)
		if err != nil {
			if p.TxHash != nil {
				p.processBadTransaction(p.TxHash, err.Error())
			}
			return nil, fmt.Errorf("parse transaction error(%s)", err)
		}
		p.BlockData = &header
		p.AllPkeys = allKeys

		parsers = append(parsers, p)

		// build merkle tree
		if len(p.TxFullData) > 0 {
			dSha256Hash, err := crypto.DoubleHash(p.TxFullData)
			if err != nil {
				logger.WithFields(log.Fields{"type": consts.CryptoError, "error": err}).Error("double hashing tx full data")
				return nil, err
			}
			dSha256Hash = converter.BinToHex(dSha256Hash)
			mrklSlice = append(mrklSlice, dSha256Hash)
		}
	}

	if len(mrklSlice) == 0 {
		mrklSlice = append(mrklSlice, []byte("0"))
	}

	return &Block{
		Header:   header,
		Parsers:  parsers,
		MrklRoot: utils.MerkleTreeRoot(mrklSlice),
	}, nil
}

func ParseBlockHeader(binaryBlock *bytes.Buffer) (utils.BlockData, error) {
	var block utils.BlockData
	var err error

	if binaryBlock.Len() < 9 {
		log.WithFields(log.Fields{"size": binaryBlock.Len()}).Error("binary block size is too small")
		return utils.BlockData{}, fmt.Errorf("bad binary block length")
	}

	blockVersion := int(converter.BinToDec(binaryBlock.Next(1)))

	if int64(binaryBlock.Len()) > syspar.GetMaxBlockSize() {
		log.WithFields(log.Fields{"size": binaryBlock.Len(), "max_size": syspar.GetMaxBlockSize()}).Error("binary block size exceeds max block size")
		err = fmt.Errorf(`len(binaryBlock) > variables.Int64["max_block_size"]  %v > %v`,
			binaryBlock.Len(), syspar.GetMaxBlockSize())

		return utils.BlockData{}, err
	}

	block.BlockID = converter.BinToDec(binaryBlock.Next(4))
	block.Time = converter.BinToDec(binaryBlock.Next(4))
	block.Version = blockVersion

	block.WalletID, err = converter.DecodeLenInt64Buf(binaryBlock)
	if err != nil {
		log.WithFields(log.Fields{"type": consts.UnmarshallingError, "block_id": block.BlockID, "block_time": block.Time, "block_version": block.Version, "error": err}).Error("decoding binary block walletID")
		return utils.BlockData{}, err
	}

	if binaryBlock.Len() < 1 {
		return utils.BlockData{}, fmt.Errorf("bad block format")
	}
	block.StateID = converter.BinToDec(binaryBlock.Next(1))
	logger := log.WithFields(log.Fields{"block_id": block.BlockID, "block_time": block.Time, "block_version": block.Version, "block_state_id": block.StateID})

	if block.BlockID > 1 {
		signSize, err := converter.DecodeLengthBuf(binaryBlock)
		if err != nil {
			logger.WithFields(log.Fields{"type": consts.UnmarshallingError, "block_id": block.BlockID, "time": block.Time, "version": block.Version, "error": err}).Error("decoding binary sign size")
			return utils.BlockData{}, err
		}
		if binaryBlock.Len() < signSize {
			logger.WithFields(log.Fields{"type": consts.UnmarshallingError, "block_id": block.BlockID, "time": block.Time, "version": block.Version, "error": err}).Error("decoding binary sign")
			return utils.BlockData{}, fmt.Errorf("bad block format (no sign)")
		}
		block.Sign = binaryBlock.Next(int(signSize))
	} else {
		binaryBlock.Next(1)
	}

	return block, nil
}

func ParseTransaction(buffer *bytes.Buffer) (*Parser, error) {
	if buffer.Len() == 0 {
		return nil, fmt.Errorf("empty transaction buffer")
	}

	hash, err := crypto.Hash(buffer.Bytes())
	// or DoubleHash ?
	if err != nil {
		log.WithFields(log.Fields{"type": consts.CryptoError, "error": err}).Error("hashing transaction")
		return nil, err
	}

	p := new(Parser)
	p.TxHash = hash
	p.TxUsedCost = decimal.New(0, 0)
	p.TxFullData = buffer.Bytes()

	txType := int64(buffer.Bytes()[0])
	p.dataType = int(txType)

	// smart contract transaction
	if IsContractTransaction(int(txType)) {
		// skip byte with transaction type
		buffer.Next(1)
		p.TxBinaryData = buffer.Bytes()
		if err := parseContractTransaction(p, buffer); err != nil {
			return nil, err
		}
		if err := p.CallContract(smart.CallInit | smart.CallCondition); err != nil {
			return nil, err
		}

		// struct transaction (only first block transaction for now)
	} else if consts.IsStruct(int(txType)) {
		p.TxBinaryData = buffer.Bytes()
		if err := parseStructTransaction(p, buffer, txType); err != nil {
			return nil, err
		}

		// all other transactions
	} else {
		// skip byte with transaction type
		buffer.Next(1)
		p.TxBinaryData = buffer.Bytes()
		if err := parseRegularTransaction(p, buffer, txType); err != nil {
			return p, err
		}
	}

	return p, nil
}

func IsContractTransaction(txType int) bool {
	return txType > 127
}

func parseContractTransaction(p *Parser, buf *bytes.Buffer) error {
	smartTx := tx.SmartContract{}
	if err := msgpack.Unmarshal(buf.Bytes(), &smartTx); err != nil {
		log.WithFields(log.Fields{"tx_type": p.dataType, "tx_hash": p.TxHash, "error": err, "type": consts.UnmarshallingError}).Error("unmarshalling smart tx msgpack")
		return err
	}
	p.TxPtr = nil
	p.TxSmart = &smartTx
	p.TxTime = smartTx.Time
	p.TxStateID = uint32(smartTx.StateID)
	p.TxStateIDStr = converter.UInt32ToStr(p.TxStateID)
	if p.TxStateID > 0 {
		p.TxCitizenID = smartTx.UserID
		p.TxWalletID = 0
	} else {
		p.TxCitizenID = 0
		p.TxWalletID = smartTx.UserID
	}

	logger := log.WithFields(log.Fields{"tx_type": p.dataType, "tx_hash": p.TxHash, "tx_time": p.TxTime, "tx_state_id": p.TxStateID, "tx_citizen_id": p.TxWalletID})

	contract := smart.GetContractByID(int32(smartTx.Type))
	if contract == nil {
		logger.WithFields(log.Fields{"contract_type": smartTx.Type}).Error("unknown contract")
		return fmt.Errorf(`unknown contract %d`, smartTx.Type)
	}
	forsign := smartTx.ForSign()

	p.TxContract = contract
	p.TxHeader = &smartTx.Header

	input := smartTx.Data
	p.TxData = make(map[string]interface{})

	if contract.Block.Info.(*script.ContractInfo).Tx != nil {
		for _, fitem := range *contract.Block.Info.(*script.ContractInfo).Tx {
			var err error
			var v interface{}
			var forv string
			var isforv bool
			switch fitem.Type.String() {
			case `uint64`:
				var val uint64
				converter.BinUnmarshal(&input, &val)
				v = val
			case `float64`:
				var val float64
				converter.BinUnmarshal(&input, &val)
				v = val
			case `int64`:
				v, err = converter.DecodeLenInt64(&input)
			case script.Decimal:
				var s string
				if err := converter.BinUnmarshal(&input, &s); err != nil {
					logger.WithFields(log.Fields{"error": err, "type": consts.UnmarshallingError}).Error("bin unmarshalling script.Decimal")
					return err
				}
				v, err = decimal.NewFromString(s)
			case `string`:
				var s string
				if err := converter.BinUnmarshal(&input, &s); err != nil {
					logger.WithFields(log.Fields{"error": err, "type": consts.UnmarshallingError}).Error("bin unmarshalling string")
					return err
				}
				v = s
			case `[]uint8`:
				var b []byte
				if err := converter.BinUnmarshal(&input, &b); err != nil {
					logger.WithFields(log.Fields{"error": err, "type": consts.UnmarshallingError}).Error("bin unmarshalling string")
					return err
				}
				v = hex.EncodeToString(b)
			case `[]interface {}`:
				count, err := converter.DecodeLength(&input)
				if err != nil {
					logger.WithFields(log.Fields{"error": err, "type": consts.UnmarshallingError}).Error("bin unmarshalling []interface{}")
					return err
				}
				isforv = true
				list := make([]interface{}, 0)
				for count > 0 {
					length, err := converter.DecodeLength(&input)
					if err != nil {
						logger.WithFields(log.Fields{"error": err, "type": consts.UnmarshallingError}).Error("bin unmarshalling tx length")
						return err
					}
					if len(input) < int(length) {
						logger.WithFields(log.Fields{"error": err, "type": consts.UnmarshallingError, "length": int(length), "slice length": len(input)}).Error("incorrect tx size")
						return fmt.Errorf(`input slice is short`)
					}
					list = append(list, string(input[:length]))
					input = input[length:]
					count--
				}
				if len(list) > 0 {
					slist := make([]string, len(list))
					for j, lval := range list {
						slist[j] = lval.(string)
					}
					forv = strings.Join(slist, `,`)
				}
				v = list
			}
			p.TxData[fitem.Name] = v
			if err != nil {
				return err
			}
			if strings.Index(fitem.Tags, `image`) >= 0 {
				continue
			}
			if isforv {
				v = forv
			}
			forsign += fmt.Sprintf(",%v", v)
		}
	}
	p.TxData[`forsign`] = forsign

	return nil
}

func parseStructTransaction(p *Parser, buf *bytes.Buffer, txType int64) error {
	trParser, err := GetParser(p, consts.TxTypes[int(txType)])
	if err != nil {
		log.WithFields(log.Fields{"error": err, "tx_type": int(txType)}).Error("getting parser for tx type")
		return err
	}
	p.txParser = trParser

	p.TxPtr = consts.MakeStruct(consts.TxTypes[int(txType)])
	input := buf.Bytes()
	if err := converter.BinUnmarshal(&input, p.TxPtr); err != nil {
		log.WithFields(log.Fields{"error": err, "type": consts.UnmarshallingError, "tx_type": int(txType)}).Error("getting parser for tx type")
		return err
	}

	head := consts.Header(p.TxPtr)
	p.TxCitizenID = head.CitizenID
	p.TxWalletID = head.WalletID
	p.TxTime = int64(head.Time)
	p.TxType = txType
	p.TxWalletID = head.WalletID
	p.TxCitizenID = head.CitizenID
	return nil
}

func parseRegularTransaction(p *Parser, buf *bytes.Buffer, txType int64) error {
	trParser, err := GetParser(p, consts.TxTypes[int(txType)])
	if err != nil {
		log.WithFields(log.Fields{"error": err, "tx_type": int(txType)}).Error("getting parser for tx type")
		return err
	}
	p.txParser = trParser

	err = trParser.Init()
	if err != nil {
		log.WithFields(log.Fields{"error": err, "tx_type": int(txType)}).Error("parser init")
		return err
	}
	header := trParser.Header()
	if header == nil {
		log.WithFields(log.Fields{"error": err, "tx_type": int(txType)}).Error("parser get header")
		return fmt.Errorf("tx header is nil")
	}

	p.TxHeader = header
	p.TxTime = header.Time
	p.TxType = txType
	p.TxStateID = uint32(header.StateID)
	p.TxUserID = header.UserID

	err = trParser.Validate()
	if _, ok := err.(error); ok {
		log.WithFields(log.Fields{"error": err, "tx_type": int(txType), "tx_time": p.TxTime, "tx_state_id": p.TxStateID, "tx_user_id": p.TxUserID}).Error("parser validate")
		return utils.ErrInfo(err.(error))
	}

	return nil
}

func checkTransaction(p *Parser, checkTime int64, checkForDupTr bool) error {
	err := CheckLogTx(p.TxFullData, checkForDupTr, false)
	if err != nil {
		return utils.ErrInfo(err)
	}
	logger := log.WithFields(log.Fields{"tx_type": p.dataType, "tx_time": p.TxTime, "tx_state_id": p.TxStateID, "tx_user_id": p.TxUserID})

	// time in the transaction cannot be more than MAX_TX_FORW seconds of block time
	if p.TxTime-consts.MAX_TX_FORW > checkTime {
		logger.WithFields(log.Fields{"tx_max_forw": consts.MAX_TX_FORW}).Error("time in the tx cannot be more than MAX_TX_FORW seconds of block time ")
		return utils.ErrInfo(fmt.Errorf("transaction time is too big"))
	}

	// time in transaction cannot be less than -24 of block time
	if p.TxTime < checkTime-consts.MAX_TX_BACK {
		logger.WithFields(log.Fields{"tx_max_back": consts.MAX_TX_BACK}).Error("time in the tx cannot be less then -24 of block time")
		return utils.ErrInfo(fmt.Errorf("incorrect transaction time"))
	}

	if p.TxContract == nil {
		if p.BlockData != nil && p.BlockData.BlockID != 1 {
			if p.TxUserID == 0 {
				logger.Error("Empty user id")
				return utils.ErrInfo(fmt.Errorf("emtpy user id"))
			}
		}
	}

	return nil
}

func CheckTransaction(data []byte) (*tx.Header, error) {
	trBuff := bytes.NewBuffer(data)
	p, err := ParseTransaction(trBuff)
	if err != nil {
		return nil, err
	}

	err = checkTransaction(p, time.Now().Unix(), true)
	if err != nil {
		return nil, err
	}

	return p.TxHeader, nil
}

func (block *Block) readPreviousBlock() error {
	if block.Header.BlockID == 1 {
		block.PrevHeader = &utils.BlockData{}
		return nil
	}

	var err error
	block.PrevHeader, err = GetBlockDataFromBlockChain(block.Header.BlockID - 1)
	if err != nil {
		return utils.ErrInfo(fmt.Errorf("can't get block %d", block.Header.BlockID-1))
	}

	return nil
}

func playTransaction(p *Parser) error {
	// smart-contract
	if p.TxContract != nil {
		// check that there are enough money in CallContract
		if err := p.CallContract(smart.CallInit | smart.CallCondition | smart.CallAction); err != nil {
			return utils.ErrInfo(err)
		}

	} else {
		if p.txParser == nil {
			return utils.ErrInfo(fmt.Errorf("can't find parser for %d", p.TxType))
		}

		err := p.txParser.Action()
		if _, ok := err.(error); ok {
			return utils.ErrInfo(err.(error))
		}
	}
	return nil
}

func (block *Block) playBlock(dbTransaction *model.DbTransaction) error {
	logger := block.GetLogger()
	if _, err := model.DeleteUsedTransactions(dbTransaction); err != nil {
		logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("delete used transactions")
		return err
	}

	for _, p := range block.Parsers {
		p.DbTransaction = dbTransaction

		if err := playTransaction(p); err != nil {
			// skip this transaction
			model.MarkTransactionUsed(nil, p.TxHash)
			p.processBadTransaction(p.TxHash, err.Error())
			continue
		}

		if _, err := model.MarkTransactionUsed(p.DbTransaction, p.TxHash); err != nil {
			logger.WithFields(log.Fields{"type": consts.DBError, "error": err, "tx_hash": p.TxHash}).Error("marking transaction used")
			return err
		}

		// update status
		ts := &model.TransactionStatus{}
		if err := ts.UpdateBlockID(p.DbTransaction, block.Header.BlockID, p.TxHash); err != nil {
			logger.WithFields(log.Fields{"type": consts.DBError, "error": err, "tx_hash": p.TxHash}).Error("updating transaction status block id")
			return err
		}
		if err := InsertInLogTx(p.DbTransaction, p.TxFullData, p.TxTime); err != nil {
			return utils.ErrInfo(err)
		}
	}
	return nil
}

func (block *Block) CheckBlock() error {
	logger := block.GetLogger()
	// exclude blocks from future
	if block.Header.Time > time.Now().Unix() {
		logger.Error("block time is larger than now")
		utils.ErrInfo(fmt.Errorf("incorrect block time"))
	}
	// is this block too early? Allowable error = error_time
	if block.PrevHeader != nil {
		if block.Header.BlockID != block.PrevHeader.BlockID+1 {
			logger.Error("block id is larger then previous more than on 1")
			return utils.ErrInfo(fmt.Errorf("incorrect block_id %d != %d +1", block.Header.BlockID, block.PrevHeader.BlockID))
		}
		// check time interval between blocks
		sleepTime, err := model.GetSleepTime(block.Header.WalletID, block.Header.StateID, block.PrevHeader.StateID, block.PrevHeader.WalletID)
		if err != nil {
			logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting sleep time")
			return utils.ErrInfo(err)
		}

		if block.PrevHeader.Time+sleepTime-block.Header.Time > consts.ERROR_TIME {
			logger.Error("incorrect block time")
			return utils.ErrInfo(fmt.Errorf("incorrect block time"))
		}
	}

	// check each transaction
	txCounter := make(map[int64]int)
	txHashes := make(map[string]struct{})
	for _, p := range block.Parsers {
		hexHash := string(converter.BinToHex(p.TxHash))
		// check for duplicate transactions
		if _, ok := txHashes[hexHash]; ok {
			logger.WithFields(log.Fields{"tx_hash": hexHash}).Error("duplicate transaction")
			return utils.ErrInfo(fmt.Errorf("duplicate transaction %s", hexHash))
		}
		txHashes[hexHash] = struct{}{}

		// check for max transaction per user in one block
		txCounter[p.TxUserID]++
		if txCounter[p.TxUserID] > syspar.GetMaxBlockUserTx() {
			logger.WithFields(log.Fields{"user_tx": txCounter[p.TxUserID], "max_user_tx": syspar.GetMaxBlockUserTx(), "tx_user_id": p.TxUserID}).Error("user with id exceed max user transactions per block")
			return utils.ErrInfo(fmt.Errorf("max_block_user_transactions"))
		}

		if err := checkTransaction(p, block.Header.Time, false); err != nil {
			return utils.ErrInfo(err)
		}

	}

	result, err := block.CheckHash()
	if err != nil {
		return utils.ErrInfo(err)
	}
	if !result {
		logger.Error("incorrect signature")
		return fmt.Errorf("incorrect signature / p.PrevBlock.BlockId: %d", block.PrevHeader.BlockID)
	}
	return nil
}

func (block *Block) CheckHash() (bool, error) {
	logger := block.GetLogger()
	if block.Header.BlockID == 1 {
		return true, nil
	}
	// check block signature
	if block.PrevHeader != nil {
		nodePublicKey, err := GetNodePublicKeyWalletOrCB(block.Header.WalletID, block.Header.StateID)
		if err != nil {
			logger.Error("getting node public key wallet or CB")
			return false, utils.ErrInfo(err)
		}
		if len(nodePublicKey) == 0 {
			logger.Error("node public key is empty")
			return false, utils.ErrInfo(fmt.Errorf("empty nodePublicKey"))
		}
		// check the signature
		forSign := fmt.Sprintf("0,%d,%s,%d,%d,%d,%s", block.Header.BlockID, block.PrevHeader.Hash,
			block.Header.Time, block.Header.WalletID, block.Header.StateID, block.MrklRoot)

		resultCheckSign, err := utils.CheckSign([][]byte{nodePublicKey}, forSign, block.Header.Sign, true)
		if err != nil {
			logger.WithFields(log.Fields{"error": err, "type": consts.CryptoError}).Error("checking block header sign")
			return false, utils.ErrInfo(fmt.Errorf("err: %v / p.PrevBlock.BlockId: %d", err, block.PrevHeader.BlockID))
		}

		return resultCheckSign, nil
	}

	return true, nil
}

func MarshallBlock(header *utils.BlockData, trData [][]byte, prevHash []byte, key string) ([]byte, error) {
	var mrklArray [][]byte
	var blockDataTx []byte
	var signed []byte
	logger := log.WithFields(log.Fields{"block_id": header.BlockID, "block_hash": header.Hash, "block_time": header.Time, "block_version": header.Version, "block_wallet_id": header.WalletID, "block_state_id": header.StateID})

	for _, tr := range trData {
		doubleHash, err := crypto.DoubleHash(tr)
		if err != nil {
			logger.WithFields(log.Fields{"type": consts.CryptoError, "error": err}).Error("double hashing transaction")
			return nil, err
		}
		mrklArray = append(mrklArray, converter.BinToHex(doubleHash))
		blockDataTx = append(blockDataTx, converter.EncodeLengthPlusData(tr)...)
	}

	if key != "" {
		if len(mrklArray) == 0 {
			mrklArray = append(mrklArray, []byte("0"))
		}
		mrklRoot := utils.MerkleTreeRoot(mrklArray)

		forSign := fmt.Sprintf("0,%d,%s,%d,%d,%d,%s",
			header.BlockID, prevHash, header.Time, header.WalletID, header.StateID, mrklRoot)

		var err error
		signed, err = crypto.Sign(key, forSign)
		if err != nil {
			logger.WithFields(log.Fields{"type": consts.CryptoError, "error": err}).Error("signing blocko")
			return nil, err
		}
	}

	var buf bytes.Buffer
	// fill header
	buf.Write(converter.DecToBin(header.Version, 1))
	buf.Write(converter.DecToBin(header.BlockID, 4))
	buf.Write(converter.DecToBin(header.Time, 4))
	buf.Write(converter.EncodeLenInt64InPlace(header.WalletID))
	buf.Write(converter.DecToBin(header.StateID, 1))
	buf.Write(converter.EncodeLengthPlusData(signed))
	buf.Write(blockDataTx)

	return buf.Bytes(), nil
}
