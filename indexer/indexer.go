package indexer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	ethCommon "github.com/ethereum/go-ethereum/common"
	ethTypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/quantity"
	"github.com/oasisprotocol/oasis-core/go/common/service"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/block"
	"github.com/oasisprotocol/oasis-sdk/client-sdk/go/client"
	"github.com/oasisprotocol/oasis-sdk/client-sdk/go/modules/core"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/oasisprotocol/emerald-web3-gateway/conf"
	"github.com/oasisprotocol/emerald-web3-gateway/db/model"
	"github.com/oasisprotocol/emerald-web3-gateway/filters"
	"github.com/oasisprotocol/emerald-web3-gateway/storage"
)

const (
	indexerLoopDelay                 = 1 * time.Second
	pruningCheckInterval             = 60 * time.Second
	healthCheckInterval              = 10 * time.Second
	healthCheckIndexerDriftThreshold = 10

	blockIndexTimeout      = 60 * time.Second
	pruneIterationTimeout  = 60 * time.Second
	healthIterationTimeout = 60 * time.Second
)

var (
	metricBlockIndexed = promauto.NewGauge(prometheus.GaugeOpts{Name: "oasis_emerald_web3_gateway_block_indexed", Help: "Indexed block heights."})
	metricBlockPruned  = promauto.NewGauge(prometheus.GaugeOpts{Name: "oasis_emerald_web3_gateway_block_pruned", Help: "Pruned block heights."})
	metricHealthy      = promauto.NewGauge(prometheus.GaugeOpts{Name: "oasis_emerald_web3_gateway_health", Help: "1 if gateway healthcheck is reporting as healthy, 0 otherwise."})
)

// ErrNotHealthy is the error returned if the gateway is unhealthy.
var ErrNotHealthy = errors.New("not healthy")

// Service is an indexer service.
type Service struct {
	service.BaseBackgroundService

	runtimeID       common.Namespace
	enablePruning   bool
	pruningStep     uint64
	indexingStart   uint64
	indexingDisable bool
	indexingFollow  bool

	backend Backend
	client  client.RuntimeClient
	storage storage.Storage
	core    core.V1

	queryBlockGasLimit bool
	blockGasLimit      uint64

	ctx       context.Context
	cancelCtx context.CancelFunc

	health uint32
}

// Implements server.HealthCheck.
func (s *Service) Health() error {
	if atomic.LoadUint32(&s.health) == 0 {
		return ErrNotHealthy
	}
	return nil
}

// indexBlock indexes given block number.
func (s *Service) indexBlock(ctx context.Context, round uint64) error {
	blk, err := s.client.GetBlock(ctx, round)
	if err != nil {
		return fmt.Errorf("querying block: %w", err)
	}

	if s.blockGasLimit == 0 || s.queryBlockGasLimit {
		// Query parameters for block gas limit.
		var params *core.Parameters
		params, err = s.core.Parameters(ctx, round)
		if err != nil {
			return fmt.Errorf("querying block parameters: %w", err)
		}
		s.blockGasLimit = params.MaxBatchGas
	}

	txs, err := s.client.GetTransactionsWithResults(ctx, blk.Header.Round)
	if err != nil {
		return fmt.Errorf("querying transactions with results: %w", err)
	}

	err = s.backend.Index(ctx, blk, txs, s.blockGasLimit)
	if err != nil {
		return fmt.Errorf("indexing block: %w", err)
	}
	metricBlockIndexed.Set(float64(blk.Header.Round))

	switch {
	case blk.Header.HeaderType == block.EpochTransition:
		// Epoch transition block, ensure block gas is queried on next normal round.
		s.queryBlockGasLimit = true
	case s.queryBlockGasLimit && blk.Header.HeaderType == block.Normal:
		// Block gas was queried, no need to query it until next epoch.
		s.queryBlockGasLimit = false
	default:
	}

	return nil
}

// getRoundLatest returns the latest round.
func (s *Service) getRoundLatest() (uint64, error) {
	blk, err := s.client.GetBlock(s.ctx, client.RoundLatest)
	if err != nil {
		return 0, err
	}

	return blk.Header.Round, nil
}

func (s *Service) updateHealth(healthy bool) {
	if healthy {
		atomic.StoreUint32(&s.health, 1)
		metricHealthy.Set(float64(1))
	} else {
		atomic.StoreUint32(&s.health, 0)
		metricHealthy.Set(float64(0))
	}
}

// healthWorker is the health check worker.
func (s *Service) healthWorker() {
	s.Logger.Debug("starting health check worker")
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(healthCheckInterval):
			func() {
				ctx, cancel := context.WithTimeout(s.ctx, healthIterationTimeout)
				defer cancel()

				// Query last indexed round.
				lastIndexed, err := s.backend.QueryLastIndexedRound(ctx)
				if err != nil {
					s.Logger.Error("failed to query last indexed round",
						"err", err,
					)
					s.updateHealth(false)
					return
				}

				// Query latest round on the node.
				latestBlk, err := s.client.GetBlock(ctx, client.RoundLatest)
				if err != nil {
					s.Logger.Error("failed to query latest block",
						"err", err,
					)
					s.updateHealth(false)
					return
				}
				latestRound := latestBlk.Header.Round

				s.Logger.Debug("checking health", "latest_round", latestRound, "latest_indexed", lastIndexed)
				switch {
				case latestRound >= lastIndexed:
					s.updateHealth(latestRound-lastIndexed <= healthCheckIndexerDriftThreshold)
				default:
					// Indexer in front of node - not healthy.
					s.updateHealth(false)
				}
			}()
		}
	}
}

// pruningWorker handles data pruning.
func (s *Service) pruningWorker() {
	s.Logger.Debug("starting periodic pruning worker")

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(pruningCheckInterval):
			func() {
				ctx, cancel := context.WithTimeout(s.ctx, pruneIterationTimeout)
				defer cancel()

				lastIndexed, err := s.backend.QueryLastIndexedRound(ctx)
				if err != nil {
					s.Logger.Error("failed to query last indexed round",
						"err", err,
					)
					return
				}

				if lastIndexed > s.pruningStep {
					round := lastIndexed - s.pruningStep
					if err := s.backend.Prune(ctx, round); err != nil {
						s.Logger.Error("failed to prune round",
							"err", err,
							"round", round,
						)
						return
					}
					metricBlockPruned.Set(float64(round))
				}
			}()
		}
	}
}

// indexingWorker is a worker for indexing.
func (s *Service) indexingWorker() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(indexerLoopDelay):
		}

		func() {
			ctx, cancel := context.WithTimeout(s.ctx, blockIndexTimeout)
			defer cancel()

			// Query latest round available at the node.
			latest, err := s.getRoundLatest()
			if err != nil {
				s.Logger.Info("failed to query latest round",
					"err", err,
				)
				return
			}

			var startAt uint64
			// Get last indexed round.
			lastIndexed, err := s.backend.QueryLastIndexedRound(ctx)
			switch {
			case errors.Is(err, storage.ErrNoRoundsIndexed):
				// No rounds indexed, start at 0.
				startAt = 0
			case err != nil:
				s.Logger.Error("failed to query last indexed round",
					"err", err,
				)
				return
			default:
				if latest < lastIndexed {
					panic("This is a new chain, please clear the db first!")
				}
				// Latest round already indexed.
				if latest == lastIndexed {
					return
				}
				startAt = lastIndexed + 1
			}

			// If a special indexing_start is set, skip ahead to that.
			if s.indexingStart > startAt {
				startAt = s.indexingStart
			}

			// Get last retained round on the node.
			lastRetainedBlock, err := s.client.GetLastRetainedBlock(ctx)
			if err != nil {
				s.Logger.Error("failed to retrieve last retained round",
					"err", err,
				)
				return
			}
			// Adjust startAt round in case node pruned missing rounds.
			if lastRetainedBlock.Header.Round > startAt {
				startAt = lastRetainedBlock.Header.Round
			}
			// Following code uses a new context with new timeout.
			cancel()

			for round := startAt; round <= latest; round++ {
				select {
				case <-s.ctx.Done():
					return
				default:
				}

				indexCtx, cancel := context.WithTimeout(s.ctx, blockIndexTimeout)
				// Try to index block.
				if err = s.indexBlock(indexCtx, round); err != nil {
					s.Logger.Warn("failed to index block",
						"err", err,
						"round", round,
					)
					cancel()
					break
				}
				cancel()
			}
		}()
	}
}

func (s *Service) followingWorker() {
	// This is gross but there is no other way to dispatch pubsub events
	// which is the whole point of this routine.
	ib := s.backend.(*indexBackend)

	var lastIndexed uint64
pollLoop:
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(indexerLoopDelay):
		}

		// Poll the db for new blocks.
		latest, err := s.storage.GetLatestBlockNumber(s.ctx)
		if err != nil {
			s.Logger.Warn("failed to query latest block from DB",
				"err", err,
			)
			continue
		}

		// Do the "right thing(TM)".
		var startAt uint64
		switch lastIndexed {
		case latest:
			// No new blocks.
			continue
		case 0:
			// First time through the loop.
			startAt = latest
		default:
			startAt = lastIndexed + 1
		}

		for round := startAt; round <= latest; round++ {
			block, err := s.storage.GetBlockByNumber(s.ctx, round)
			if err != nil {
				s.Logger.Warn("failed to get block from DB",
					"err", err,
					"round", round,
				)
				continue pollLoop
			}

			logs, err := s.storage.GetLogs(s.ctx, round, round)
			if err != nil {
				s.Logger.Warn("failed to get logs from DB",
					"err", err,
					"round", round,
				)
				continue pollLoop
			}

			chainEvent := filters.ChainEvent{
				Block: block,
				Hash:  ethCommon.HexToHash(block.Hash),
				Logs:  logs,
			}
			ib.subscribe.ChainChan() <- chainEvent

			// Reconstruct the transactions and receipts that were actually
			// stored for this block.  The replacement of prior failure
			// nonsense is surprisingly annoying to deal with.
			numTxes := len(block.Transactions)
			uniqueTxes := make([]*model.Transaction, 0, numTxes)
			uniqueReceipts := make([]*model.Receipt, 0, numTxes)
			var lastTransactionPrice quantity.Quantity
			for i, tx := range block.Transactions {
				// In the Emerald Paratime it can happen that an already committed
				// transaction is re-proposed at a later block. The duplicate transaction fails,
				// but it is still included in the block. The gateway should drop these
				// transactions to remain compatible with ETH semantics.
				if tx.Status == uint(ethTypes.ReceiptStatusFailed) {
					continue
				}

				dbTx, err := s.storage.GetTransaction(s.ctx, tx.Hash)
				if err != nil {
					// Unlike the indexer version of this, the tx (by hash)
					// MUST be in the database by the point that the block
					// is in the database.
					//
					// This is an invariant violation so recovery is unlikely.
					s.Logger.Warn("failed to get tx from DB",
						"err", err,
						"round", round,
						"tx_hash", tx.Hash,
					)
					continue pollLoop
				}
				if dbTx.Round != tx.Round && dbTx.Status != uint(ethTypes.ReceiptStatusFailed) {
					// Earlier tx exists, and it didn't fail.
					//
					// This is an invariant violation so recovery is unlikely.
					ib.logger.Error("duplicate tx",
						"round", round,
						"earlier_tx", dbTx,
						"tx", tx,
					)
					continue pollLoop
				}

				// If the tx passes the deduplication/replacement checks,
				// presumably the receipt does as well?
				receipt, err := s.storage.GetTransactionReceipt(s.ctx, tx.Hash)
				if err != nil {
					s.Logger.Warn("failed to get tx receipt from DB",
						"err", err,
						"round", round,
						"tx_hash", tx.Hash,
					)
					continue pollLoop
				}

				// Done this way because quantity.Quantity.UnmarshalText
				// can have a side-effect on error.
				if err = func() error {
					var gp big.Int
					if err = gp.UnmarshalText([]byte(tx.GasPrice)); err != nil {
						return err
					}
					return lastTransactionPrice.FromBigInt(&gp)
				}(); err != nil {
					s.Logger.Warn("failed to parse tx gas price",
						"err", err,
						"round", round,
						"tx_hash", tx.Hash,
					)
				}

				uniqueTxes = append(uniqueTxes, block.Transactions[i])
				uniqueReceipts = append(uniqueReceipts, receipt)
			}

			blockData := &BlockData{
				Block:                block,
				Receipts:             uniqueReceipts,
				UniqueTxes:           uniqueTxes,
				LastTransactionPrice: &lastTransactionPrice,
			}

			if ib.observer != nil {
				ib.observer.OnBlockIndexed(blockData)
			}
			ib.blockNotifier.Broadcast(blockData)
		}

		lastIndexed = latest // Caught up!
	}
}

// Start starts service.
func (s *Service) Start() {
	switch {
	case s.indexingDisable:
		// Archive mode disables the worker entirely.
		s.updateHealth(true)
		return
	case s.indexingFollow:
		// DB follow mode follows another indexer instance via the DB.
		s.updateHealth(true)
		go s.followingWorker()
		return
	default:
	}

	go s.indexingWorker()
	go s.healthWorker()

	if s.enablePruning {
		go s.pruningWorker()
	}
}

// Stop stops service.
func (s *Service) Stop() {
	s.cancelCtx()
}

// New creates a new indexer service.
func New(
	ctxBackend context.Context,
	backend Backend,
	client client.RuntimeClient,
	runtimeID common.Namespace,
	storage storage.Storage,
	cfg *conf.Config,
) (*Service, Backend, error) {
	ctx, cancelCtx := context.WithCancel(ctxBackend)
	cachingBackend := newCachingBackend(ctx, backend, cfg.Cache)

	s := &Service{
		BaseBackgroundService: *service.NewBaseBackgroundService("gateway/indexer"),
		runtimeID:             runtimeID,
		backend:               cachingBackend,
		client:                client,
		storage:               storage,
		core:                  core.NewV1(client),
		ctx:                   ctx,
		cancelCtx:             cancelCtx,
		enablePruning:         cfg.EnablePruning,
		pruningStep:           cfg.PruningStep,
		indexingStart:         cfg.IndexingStart,
		indexingDisable:       cfg.IndexingDisable,
		indexingFollow:        cfg.IndexingSQLFollow,
	}
	s.Logger = s.Logger.With("runtime_id", s.runtimeID.String())

	// TODO/NotYawning: Non-archive nodes probably want to do something
	// different here.
	if s.indexingDisable {
		if _, err := s.backend.QueryLastIndexedRound(ctx); err != nil {
			s.Logger.Error("indexer disabled and no rounds indexed, this will never work",
				"err", err,
			)
			return nil, nil, err
		}
	}

	return s, cachingBackend, nil
}
