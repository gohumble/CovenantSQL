/*
 * Copyright 2018 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package worker

import (
	"os"
	"path/filepath"
	//"runtime/trace"
	"sync"
	"time"

	"github.com/CovenantSQL/CovenantSQL/crypto/kms"
	"github.com/CovenantSQL/CovenantSQL/kayak"
	kt "github.com/CovenantSQL/CovenantSQL/kayak/types"
	kl "github.com/CovenantSQL/CovenantSQL/kayak/wal"
	"github.com/CovenantSQL/CovenantSQL/proto"
	"github.com/CovenantSQL/CovenantSQL/sqlchain"
	"github.com/CovenantSQL/CovenantSQL/storage"
	"github.com/CovenantSQL/CovenantSQL/types"
	"github.com/pkg/errors"
)

const (
	// StorageFileName defines storage file name of database instance.
	StorageFileName = "storage.db3"

	// KayakWalFileName defines log pool name of database instance.
	KayakWalFileName = "kayak.ldb"

	// SQLChainFileName defines sqlchain storage file name.
	SQLChainFileName = "chain.db"

	// MaxRecordedConnectionSequences defines the max connection slots to anti reply attack.
	MaxRecordedConnectionSequences = 1000

	// PrepareThreshold defines the prepare complete threshold.
	PrepareThreshold = 1.0

	// CommitThreshold defines the commit complete threshold.
	CommitThreshold = 1.0
)

// Database defines a single database instance in worker runtime.
type Database struct {
	cfg            *DBConfig
	dbID           proto.DatabaseID
	kayakWal       *kl.LevelDBWal
	kayakRuntime   *kayak.Runtime
	kayakConfig    *kt.RuntimeConfig
	connSeqs       sync.Map
	connSeqEvictCh chan uint64
	chain          *sqlchain.Chain
	nodeID         proto.NodeID
	mux            *DBKayakMuxService
}

// NewDatabase create a single database instance using config.
func NewDatabase(cfg *DBConfig, peers *proto.Peers,
	profile *types.SQLChainProfile) (db *Database, err error) {
	// ensure dir exists
	if err = os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return
	}

	if peers == nil || profile.Genesis == nil {
		err = ErrInvalidDBConfig
		return
	}

	// init database
	db = &Database{
		cfg:            cfg,
		dbID:           cfg.DatabaseID,
		mux:            cfg.KayakMux,
		connSeqEvictCh: make(chan uint64, 1),
	}

	defer func() {
		// on error recycle all resources
		if err != nil {
			// stop kayak runtime
			if db.kayakRuntime != nil {
				db.kayakRuntime.Shutdown()
			}

			// close chain
			if db.chain != nil {
				db.chain.Stop()
			}
		}
	}()

	// init storage
	storageFile := filepath.Join(cfg.DataDir, StorageFileName)
	storageDSN, err := storage.NewDSN(storageFile)
	if err != nil {
		return
	}

	if cfg.EncryptionKey != "" {
		storageDSN.AddParam("_crypto_key", cfg.EncryptionKey)
	}

	// init chain
	chainFile := filepath.Join(cfg.DataDir, SQLChainFileName)
	if db.nodeID, err = kms.GetLocalNodeID(); err != nil {
		return
	}

	// TODO(xq262144): make sqlchain config use of global config object
	chainCfg := &sqlchain.Config{
		DatabaseID:      cfg.DatabaseID,
		ChainFilePrefix: chainFile,
		DataFile:        storageDSN.Format(),
		Genesis:         profile.Genesis,
		Peers:           peers,

		// TODO(xq262144): should refactor server/node definition to conf/proto package
		// currently sqlchain package only use Server.ID as node id
		MuxService: cfg.ChainMux,
		Server:     db.nodeID,

		// TODO(xq262144): currently using fixed period/resolution from sqlchain test case
		Period:   60 * time.Second,
		Tick:     10 * time.Second,
		QueryTTL: 10,

		Profile: profile,
	}
	if db.chain, err = sqlchain.NewChain(chainCfg); err != nil {
		return
	} else if err = db.chain.Start(); err != nil {
		return
	}

	// init kayak config
	kayakWalPath := filepath.Join(cfg.DataDir, KayakWalFileName)
	if db.kayakWal, err = kl.NewLevelDBWal(kayakWalPath); err != nil {
		err = errors.Wrap(err, "init kayak log pool failed")
		return
	}

	db.kayakConfig = &kt.RuntimeConfig{
		Handler:          db,
		PrepareThreshold: PrepareThreshold,
		CommitThreshold:  CommitThreshold,
		PrepareTimeout:   time.Second,
		CommitTimeout:    time.Second * 60,
		Peers:            peers,
		Wal:              db.kayakWal,
		NodeID:           db.nodeID,
		InstanceID:       string(db.dbID),
		ServiceName:      DBKayakRPCName,
		MethodName:       DBKayakMethodName,
	}

	// create kayak runtime
	if db.kayakRuntime, err = kayak.NewRuntime(db.kayakConfig); err != nil {
		return
	}

	// register kayak runtime rpc
	db.mux.register(db.dbID, db.kayakRuntime)

	// start kayak runtime
	db.kayakRuntime.Start()

	// init sequence eviction processor
	go db.evictSequences()

	return
}

// UpdatePeers defines peers update query interface.
func (db *Database) UpdatePeers(peers *proto.Peers) (err error) {
	if err = db.kayakRuntime.UpdatePeers(peers); err != nil {
		return
	}

	return db.chain.UpdatePeers(peers)
}

// Query defines database query interface.
func (db *Database) Query(request *types.Request) (response *types.Response, err error) {
	// Just need to verify signature in db.saveAck
	//if err = request.Verify(); err != nil {
	//	return
	//}

	switch request.Header.QueryType {
	case types.ReadQuery:
		return db.chain.Query(request)
	case types.WriteQuery:
		return db.writeQuery(request)
	default:
		// TODO(xq262144): verbose errors with custom error structure
		return nil, errors.Wrap(ErrInvalidRequest, "invalid query type")
	}
}

// Ack defines client response ack interface.
func (db *Database) Ack(ack *types.Ack) (err error) {
	// Just need to verify signature in db.saveAck
	//if err = ack.Verify(); err != nil {
	//	return
	//}

	return db.saveAck(&ack.Header)
}

// Shutdown stop database handles and stop service the database.
func (db *Database) Shutdown() (err error) {
	if db.kayakRuntime != nil {
		// shutdown, stop kayak
		if err = db.kayakRuntime.Shutdown(); err != nil {
			return
		}

		// unregister
		db.mux.unregister(db.dbID)
	}

	if db.kayakWal != nil {
		// shutdown, stop kayak
		db.kayakWal.Close()
	}

	if db.chain != nil {
		// stop chain
		if err = db.chain.Stop(); err != nil {
			return
		}
	}

	if db.connSeqEvictCh != nil {
		// stop connection sequence evictions
		select {
		case _, ok := <-db.connSeqEvictCh:
			if ok {
				close(db.connSeqEvictCh)
			}
		default:
			close(db.connSeqEvictCh)
		}
	}

	return
}

// Destroy stop database instance and destroy all data/meta.
func (db *Database) Destroy() (err error) {
	if err = db.Shutdown(); err != nil {
		return
	}

	// TODO(xq262144): remove database files, now simply remove whole root dir
	os.RemoveAll(db.cfg.DataDir)

	return
}

func (db *Database) writeQuery(request *types.Request) (response *types.Response, err error) {
	//ctx := context.Background()
	//ctx, task := trace.NewTask(ctx, "writeQuery")
	//defer task.End()
	//defer trace.StartRegion(ctx, "writeQueryRegion").End()

	// check database size first, wal/kayak/chain database size is not included
	if db.cfg.SpaceLimit > 0 {
		path := filepath.Join(db.cfg.DataDir, StorageFileName)
		var statInfo os.FileInfo
		if statInfo, err = os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				return
			}
		} else {
			if uint64(statInfo.Size()) > db.cfg.SpaceLimit {
				// rejected
				err = ErrSpaceLimitExceeded
				return
			}
		}
	}

	// call kayak runtime Process
	var result interface{}
	if result, _, err = db.kayakRuntime.Apply(request.GetContext(), request); err != nil {
		err = errors.Wrap(err, "apply failed")
		return
	}

	var ok bool
	if response, ok = (result).(*types.Response); !ok {
		err = errors.Wrap(err, "invalid response type")
		return
	}

	return
}

func (db *Database) saveAck(ackHeader *types.SignedAckHeader) (err error) {
	return db.chain.VerifyAndPushAckedQuery(ackHeader)
}

func getLocalTime() time.Time {
	return time.Now().UTC()
}
