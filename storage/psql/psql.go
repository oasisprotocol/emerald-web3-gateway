package psql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/starfishlabs/oasis-evm-web3-gateway/conf"
	"github.com/starfishlabs/oasis-evm-web3-gateway/model"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

type PostDB struct {
	DB *bun.DB
}

// InitDB creates postgresql db instance.
func InitDB(cfg *conf.DatabaseConfig) (*PostDB, error) {
	if cfg == nil {
		return nil, errors.New("nil configuration")
	}

	pgConn := pgdriver.NewConnector(
		pgdriver.WithAddr(fmt.Sprintf("%v:%v", cfg.Host, cfg.Port)),
		pgdriver.WithDatabase(cfg.DB),
		pgdriver.WithUser(cfg.User),
		pgdriver.WithPassword(cfg.Password),
		pgdriver.WithDialTimeout(time.Duration(cfg.DialTimeout)*time.Second),
		pgdriver.WithReadTimeout(time.Duration(cfg.ReadTimeout)*time.Second),
		pgdriver.WithWriteTimeout(time.Duration(cfg.WriteTimeout)*time.Second))

	// open
	sqlDB := sql.OpenDB(pgConn)
	maxOpenConns := 4 * runtime.GOMAXPROCS(0)
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)

	// create db
	db := bun.NewDB(sqlDB, pgdialect.New())

	// create tables
	err := model.CreateTables(db)
	if err != nil {
		return nil, err
	}

	return &PostDB{DB: db}, nil
}

// GetTransactionRef returns block hash, round and index of the transaction.
func (db *PostDB) GetTransactionRef(txHash string) (*model.TransactionRef, error) {
	tx := new(model.TransactionRef)
	err := db.DB.NewSelect().Model(tx).Where("eth_tx_hash = ? ", txHash).Scan(context.Background())
	if err != nil {
		return nil, err
	}

	return tx, nil
}

// GetTransaction queries ethereum transaction by hash.
func (db *PostDB) GetTransaction(hash string) (*model.Transaction, error) {
	tx := new(model.Transaction)
	err := db.DB.NewSelect().Model(tx).Where("hash = ?", hash).Scan(context.Background())
	if err != nil {
		return nil, err
	}

	return tx, nil
}

// upsert updates record when PK conflicts, otherwise inserts.
func (db *PostDB) upsertSingle(value interface{}) error {
	// PKs are required for ON CONFLICT DO UPDATE
	typ := reflect.TypeOf(value)
	table := db.DB.Table(typ)
	pks := make([]string, len(table.PKs))
	for i, f := range table.PKs {
		pks[i] = f.Name
	}

	_, err := db.DB.NewInsert().Model(value).
		On(fmt.Sprintf("CONFLICT (%v) DO UPDATE", strings.Join(pks, ","))).Exec(context.Background())

	return err
}

//  upsert updates record when PK conflicts, otherwise inserts.
func (db *PostDB) upsert(value interface{}) (err error) {
	switch values := value.(type) {
	case []interface{}:
		for v := range values {
			err = db.upsertSingle(v)
		}
	case interface{}:
		err = db.upsertSingle(value)
	}

	return
}

// Store stores data in db.
func (db *PostDB) Store(value interface{}) error {
	return db.upsert(value)
}

// Update updates record.
func (db *PostDB) Update(value interface{}) error {
	return db.upsert(value)
}

// Delete deletes all records with round less than the given round.
func (db *PostDB) Delete(table interface{}, round uint64) error {
	_, err := db.DB.NewDelete().Model(table).Where("round < ?", round).Exec(context.Background())
	return err
}

// GetBlockRound returns block round by block hash.
func (db *PostDB) GetBlockRound(hash string) (uint64, error) {
	block := new(model.BlockRef)
	err := db.DB.NewSelect().Model(block).Where("hash = ?", hash).Scan(context.Background())
	if err != nil {
		return 0, err
	}

	return block.Round, nil
}

// GetBlockHash returns block hash by block round.
func (db *PostDB) GetBlockHash(round uint64) (string, error) {
	block := new(model.BlockRef)
	err := db.DB.NewSelect().Model(block).Where("round = ?", round).Scan(context.Background())
	if err != nil {
		return "", err
	}

	return block.Hash, nil
}

// GetLatestBlockHash returns for the block hash of the latest round.
func (db *PostDB) GetLatestBlockHash() (string, error) {
	block := new(model.BlockRef)
	err := db.DB.NewSelect().Model(block).Order("round DESC").Limit(1).Scan(context.Background())
	if err != nil {
		return "", err
	}

	return block.Hash, nil
}

// GetContinuesIndexedRound returns latest continues indexed block round.
func (db *PostDB) GetContinuesIndexedRound() (uint64, error) {
	indexedRound := new(model.IndexedRoundWithTip)
	err := db.DB.NewSelect().Model(indexedRound).Where("tip = ?", model.Continues).Scan(context.Background())
	if err != nil {
		return 0, err
	}

	return indexedRound.Round, nil
}

// GetLastRetainedRound returns the minimum round not pruned.
func (db *PostDB) GetLastRetainedRound() (uint64, error) {
	retainedRound := new(model.IndexedRoundWithTip)
	err := db.DB.NewSelect().Model(retainedRound).Where("tip = ?", model.LastRetained).Scan(context.Background())
	if err != nil {
		return 0, err
	}

	return retainedRound.Round, nil
}

// GetLatestBlockNumber returns the latest block number.
func (db *PostDB) GetLatestBlockNumber() (uint64, error) {
	block := new(model.Block)
	err := db.DB.NewSelect().Model(block).Order("round DESC").Limit(1).Scan(context.Background())
	if err != nil {
		return 0, err
	}

	return block.Round, nil
}

// GetBlockByHash returns the block for the given hash.
func (db *PostDB) GetBlockByHash(blockHash string) (*model.Block, error) {
	blk := new(model.Block)
	err := db.DB.NewSelect().Model(blk).Where("hash = ?", blockHash).Scan(context.Background())
	if err != nil {
		return nil, err
	}

	return blk, nil
}

// GetBlockByNumber returns the block for the given round.
func (db *PostDB) GetBlockByNumber(round uint64) (*model.Block, error) {
	block := new(model.Block)
	err := db.DB.NewSelect().Model(block).Where("round = ?", round).Scan(context.Background())
	if err != nil {
		return nil, err
	}

	return block, nil
}

// GetBlockTransactionCountByNumber returns the count of transactions in block by block number.
func (db *PostDB) GetBlockTransactionCountByNumber(round uint64) (int, error) {
	block := new(model.Block)
	err := db.DB.NewSelect().Model(block).Where("round = ?", round).Scan(context.Background())
	if err != nil {
		return 0, err
	}

	return len(block.Transactions), nil
}

// GetBlockTransactionCountByHash returns the count of transactions in block by block hash.
func (db *PostDB) GetBlockTransactionCountByHash(blockHash string) (int, error) {
	block := new(model.Block)
	err := db.DB.NewSelect().Model(block).Where("hash = ?", blockHash).Scan(context.Background())
	if err != nil {
		return 0, err
	}

	return len(block.Transactions), nil
}

// GetBlockTransaction returns transaction by bock hash and transaction index.
func (db *PostDB) GetBlockTransaction(blockHash string, txIndex int) (*model.Transaction, error) {
	block := new(model.Block)
	err := db.DB.NewSelect().Model(block).Where("hash = ?", blockHash).Scan(context.Background())
	if err != nil {
		return nil, err
	}
	if len(block.Transactions) == 0 {
		return nil, errors.New("the block doesn't have any transactions")
	}
	if len(block.Transactions)-1 < txIndex {
		return nil, errors.New("index out of range")
	}

	return block.Transactions[txIndex], nil
}

// GetTransactionReceipt returns receipt by transaction hash.
func (db *PostDB) GetTransactionReceipt(txHash string) (*model.Receipt, error) {
	receipt := new(model.Receipt)
	err := db.DB.NewSelect().Model(receipt).Where("transaction_hash = ?", txHash).Scan(context.Background())
	if err != nil {
		return nil, err
	}

	return receipt, nil
}

// GetLogs return the logs by block hash and round.
func (db *PostDB) GetLogs(startRound, endRound uint64) ([]*model.Log, error) {
	logs := []*model.Log{}
	err := db.DB.NewSelect().Model(&logs).
		Where("round BETWEEN ? AND ?", startRound, endRound).
		Scan(context.Background())
	if err != nil {
		return nil, err
	}

	return logs, nil
}
