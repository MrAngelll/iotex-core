package chainservice

import (
	"context"
	"os"

	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/actpool"
	"github.com/iotexproject/iotex-core/blockchain"
	"github.com/iotexproject/iotex-core/blocksync"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/consensus"
	"github.com/iotexproject/iotex-core/dispatcher"
	"github.com/iotexproject/iotex-core/explorer"
	explorerapi "github.com/iotexproject/iotex-core/explorer/idl/explorer"
	"github.com/iotexproject/iotex-core/indexservice"
	"github.com/iotexproject/iotex-core/logger"
	"github.com/iotexproject/iotex-core/network"
	pb "github.com/iotexproject/iotex-core/proto"
)

// ChainService is a blockchain service with all blockchain components.
type ChainService struct {
	actpool      actpool.ActPool
	blocksync    blocksync.BlockSync
	consensus    consensus.Consensus
	chain        blockchain.Blockchain
	explorer     *explorer.Server
	indexservice *indexservice.Server
}

type optionParams struct {
	rootChainAPI explorerapi.Explorer
	isTesting    bool
}

// Option sets ChainService construction parameter.
type Option func(ops *optionParams) error

// WithRootChainAPI is an option to add a root chain api to ChainService.
func WithRootChainAPI(exp explorerapi.Explorer) Option {
	return func(ops *optionParams) error {
		ops.rootChainAPI = exp
		return nil
	}
}

// WithTesting is an option to create a testing ChainService.
func WithTesting() Option {
	return func(ops *optionParams) error {
		ops.isTesting = true
		return nil
	}
}

// New creates a ChainService from config and network.Overlay and dispatcher.Dispatcher.
func New(cfg *config.Config, p2p network.Overlay, dispatcher dispatcher.Dispatcher, opts ...Option) (*ChainService, error) {
	var ops optionParams
	for _, opt := range opts {
		if err := opt(&ops); err != nil {
			return nil, err
		}
	}

	var chainOpts []blockchain.Option
	if ops.isTesting {
		chainOpts = []blockchain.Option{blockchain.InMemStateFactoryOption(), blockchain.InMemDaoOption()}
	} else {
		chainOpts = []blockchain.Option{blockchain.DefaultStateFactoryOption(), blockchain.BoltDBDaoOption()}
	}

	// create Blockchain
	chain := blockchain.NewBlockchain(cfg, chainOpts...)
	if chain == nil && cfg.Chain.EnableFallBackToFreshDB {
		logger.Warn().Msg("Chain db and trie db are falling back to fresh ones")
		if err := os.Rename(cfg.Chain.ChainDBPath, cfg.Chain.ChainDBPath+".old"); err != nil {
			return nil, errors.Wrap(err, "failed to rename old chain db")
		}
		if err := os.Rename(cfg.Chain.TrieDBPath, cfg.Chain.TrieDBPath+".old"); err != nil {
			return nil, errors.Wrap(err, "failed to rename old trie db")
		}
		chain = blockchain.NewBlockchain(cfg, blockchain.DefaultStateFactoryOption(), blockchain.BoltDBDaoOption())
	}

	// Create ActPool
	actPool, err := actpool.NewActPool(chain, cfg.ActPool)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create actpool")
	}
	bs, err := blocksync.NewBlockSyncer(cfg, chain, actPool, p2p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create blockSyncer")
	}

	var copts []consensus.Option
	if ops.rootChainAPI != nil {
		copts = []consensus.Option{consensus.WithRootChainAPI(ops.rootChainAPI)}
	}
	consensus := consensus.NewConsensus(cfg, chain, actPool, p2p, copts...)
	if consensus == nil {
		return nil, errors.Wrap(err, "failed to create consensus")
	}

	var idx *indexservice.Server
	if cfg.Indexer.Enabled {
		idx = indexservice.NewServer(cfg, chain)
		if idx == nil {
			return nil, errors.Wrap(err, "failed to create index service")
		}
	} else {
		idx = nil
	}

	var exp *explorer.Server
	if cfg.Explorer.IsTest || os.Getenv("APP_ENV") == "development" {
		logger.Warn().Msg("Using test server with fake data...")
		exp = explorer.NewTestSever(cfg.Explorer)
	} else {
		exp = explorer.NewServer(cfg.Explorer, chain, consensus, dispatcher, actPool, p2p)
	}
	return &ChainService{
		actpool:      actPool,
		chain:        chain,
		blocksync:    bs,
		consensus:    consensus,
		indexservice: idx,
		explorer:     exp,
	}, nil
}

// Start starts the server
func (cs *ChainService) Start(ctx context.Context) error {
	if err := cs.chain.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting blockchain")
	}
	if err := cs.consensus.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting consensus")
	}
	if err := cs.blocksync.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting blocksync")
	}

	if cs.indexservice != nil {
		if err := cs.indexservice.Start(ctx); err != nil {
			return errors.Wrap(err, "error when starting indexservice")
		}
	}

	if err := cs.explorer.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting explorer")
	}
	return nil
}

// Stop stops the server
func (cs *ChainService) Stop(ctx context.Context) error {
	if err := cs.explorer.Stop(ctx); err != nil {
		return errors.Wrap(err, "error when stopping explorer")
	}

	if cs.indexservice != nil {
		if err := cs.indexservice.Stop(ctx); err != nil {
			return errors.Wrap(err, "error when stopping indexservice")
		}
	}

	if err := cs.consensus.Stop(ctx); err != nil {
		return errors.Wrap(err, "error when stopping consensus")
	}
	if err := cs.blocksync.Stop(ctx); err != nil {
		return errors.Wrap(err, "error when stopping blocksync")
	}
	if err := cs.chain.Stop(ctx); err != nil {
		return errors.Wrap(err, "error when stopping blockchain")
	}
	return nil
}

// HandleAction handles incoming action request.
func (cs *ChainService) HandleAction(act *pb.ActionPb) error {
	if pbTsf := act.GetTransfer(); pbTsf != nil {
		tsf := &action.Transfer{}
		tsf.ConvertFromActionPb(act)
		if err := cs.actpool.AddTsf(tsf); err != nil {
			logger.Debug().Err(err)
			return err
		}
	} else if pbVote := act.GetVote(); pbVote != nil {
		vote := &action.Vote{}
		vote.ConvertFromActionPb(act)
		if err := cs.actpool.AddVote(vote); err != nil {
			logger.Debug().Err(err)
			return err
		}
	} else if pbExecution := act.GetExecution(); pbExecution != nil {
		execution := &action.Execution{}
		execution.ConvertFromActionPb(act)
		if err := cs.actpool.AddExecution(execution); err != nil {
			logger.Debug().Err(err).Msg("Failed to add execution")
			return err
		}
	}
	return nil
}

// HandleBlock handles incoming block request.
func (cs *ChainService) HandleBlock(pbBlock *pb.BlockPb) error {
	blk := &blockchain.Block{}
	blk.ConvertFromBlockPb(pbBlock)
	return cs.blocksync.ProcessBlock(blk)
}

// HandleBlockSync handles incoming block sync request.
func (cs *ChainService) HandleBlockSync(pbBlock *pb.BlockPb) error {
	blk := &blockchain.Block{}
	blk.ConvertFromBlockPb(pbBlock)
	return cs.blocksync.ProcessBlockSync(blk)
}

// HandleSyncRequest handles incoming sync request.
func (cs *ChainService) HandleSyncRequest(sender string, sync *pb.BlockSync) error {
	return cs.blocksync.ProcessSyncRequest(sender, sync)
}

// HandleBlockPropose handles incoming block propose request.
func (cs *ChainService) HandleBlockPropose(propose *pb.ProposePb) error {
	return cs.consensus.HandleBlockPropose(propose)
}

// HandleEndorse handles incoming endorse request.
func (cs *ChainService) HandleEndorse(endorse *pb.EndorsePb) error {
	return cs.consensus.HandleEndorse(endorse)
}

// ChainID returns ChainID.
func (cs *ChainService) ChainID() uint32 { return cs.chain.ChainID() }

// Blockchain returns the Blockchain
func (cs *ChainService) Blockchain() blockchain.Blockchain {
	return cs.chain
}

// ActionPool returns the Action pool
func (cs *ChainService) ActionPool() actpool.ActPool {
	return cs.actpool
}

// Consensus returns the consensus instance
func (cs *ChainService) Consensus() consensus.Consensus {
	return cs.consensus
}

// BlockSync returns the block syncer
func (cs *ChainService) BlockSync() blocksync.BlockSync {
	return cs.blocksync
}

// IndexService returns the indexservice instance
func (cs *ChainService) IndexService() *indexservice.Server {
	return cs.indexservice
}

// Explorer returns the explorer instance
func (cs *ChainService) Explorer() *explorer.Server {
	return cs.explorer
}
