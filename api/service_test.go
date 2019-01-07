package api_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/CovenantSQL/CovenantSQL/api"
	"github.com/CovenantSQL/CovenantSQL/api/models"
	"github.com/pkg/errors"

	"github.com/gorilla/websocket"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/sourcegraph/jsonrpc2"
	wsstream "github.com/sourcegraph/jsonrpc2/websocket"
)

const (
	bpA   = "9jt00yI91HQ4bCdFfkXWeg"
	bpB   = "3ToG8OstmKcWCzLXRy2K0w"
	addrA = "9JvxiUpBFpkUCCiYf84OCw"
	addrB = "I4TezPRXrdBZM9Mp7cr3Gw"
)

var (
	testdb, _ = filepath.Abs("./testdb.db3")

	ddls = []string{
		`CREATE TABLE IF NOT EXISTS "indexed_blocks" (
			"height"		INTEGER PRIMARY KEY,
			"hash"			TEXT,
			"timestamp"		INTEGER,
			"version"		INTEGER,
			"producer"		TEXT,
			"merkle_root"	TEXT,
			"parent"		TEXT,
			"tx_count"		INTEGER
		);`,

		`CREATE INDEX IF NOT EXISTS "idx__indexed_blocks__hash" ON "indexed_blocks" ("hash");`,
		`CREATE INDEX IF NOT EXISTS "idx__indexed_blocks__timestamp" ON "indexed_blocks" ("timestamp" DESC);`,

		`CREATE TABLE IF NOT EXISTS "indexed_transactions" (
			"block_height"	INTEGER,
			"tx_index"		INTEGER,
			"hash"			TEXT,
			"block_hash"	TEXT,
			"timestamp"		INTEGER,
			"tx_type"		INTEGER,
			"address"		TEXT,
			"raw"			TEXT,
			PRIMARY KEY ("block_height", "tx_index")
		);`,

		`CREATE INDEX IF NOT EXISTS "idx__indexed_transactions__hash" ON "indexed_transactions" ("hash");`,
		`CREATE INDEX IF NOT EXISTS "idx__indexed_transactions__block_hash" ON "indexed_transactions" ("block_hash");`,
		`CREATE INDEX IF NOT EXISTS "idx__indexed_transactions__timestamp" ON "indexed_transactions" ("timestamp" DESC);`,
		`CREATE INDEX IF NOT EXISTS "idx__indexed_transactions__tx_type__timestamp" ON "indexed_transactions" ("tx_type", "timestamp" DESC);`,
		`CREATE INDEX IF NOT EXISTS "idx__indexed_transactions__address__timestamp" ON "indexed_transactions" ("address", "timestamp" DESC);`,
	}

	blocksMockData = [][]interface{}{
		{1, "HGGcDJqO7tuZWwJyFxRl9g", 1546589042828174631, 1, bpA, "apple", "0000000000000000000000", 0},
		{2, "pfp8ZcSwhg15W2YSaooX8g", 1546589042482919184, 1, bpA, "apple", "HGGcDJqO7tuZWwJyFxRl9g", 1},
		{3, "NP5Ze1z8hfdG5_G8StXYLw", 1546589042010844731, 1, bpA, "apple", "pfp8ZcSwhg15W2YSaooX8g", 0},
		{4, "gZpo0Y_Wh9u6TxAnFWmiMQ", 1546589042185749429, 1, bpA, "apple", "NP5Ze1z8hfdG5_G8StXYLw", 0},
		{5, "mXMsSXd0OY5MocYl3b5r4Q", 1546589042858585920, 1, bpA, "apple", "gZpo0Y_Wh9u6TxAnFWmiMQ", 0},
		{6, "K7aFl5KIW_xKrUmfpJt6Zg", 1546590006812948193, 1, bpB, "google", "mXMsSXd0OY5MocYl3b5r4Q", 0},
		{7, "iTbk_EvsiprSwLLpC9LOgg", 1546590006885392010, 1, bpB, "google", "K7aFl5KIW_xKrUmfpJt6Zg", 5},
		{8, "RjbeqFM8weHtCSoL_pKurQ", 1546590006585839201, 1, bpB, "google", "iTbk_EvsiprSwLLpC9LOgg", 0},
		{9, "IPS7_Ttp7vdcice8EAWx0g", 1546590006919858504, 1, bpB, "google", "RjbeqFM8weHtCSoL_pKurQ", 0},
		{10, "er05e7FvAZOP3gP5_w_RKw", 1546590006857575843, 1, bpB, "google", "IPS7_Ttp7vdcice8EAWx0g", 3},
		{11, "f0_Dk_vFItabbmcnxNxrTA", 1546590200951918474, 1, bpB, "google", "er05e7FvAZOP3gP5_w_RKw", 0},
		{12, "1pkuZ0pk1d4lzItxrA73KQ", 1546590208582918459, 1, bpB, "google", "f0_Dk_vFItabbmcnxNxrTA", 0},
		{13, "WbhKd7fPzX2Mr8JFyVOljw", 1546590200101838483, 1, bpB, "google", "1pkuZ0pk1d4lzItxrA73KQ", 0},
		{14, "niLUTZpEpOWpPx011bZGlg", 1546590200058583818, 1, bpB, "google", "WbhKd7fPzX2Mr8JFyVOljw", 0},
	}

	transactionsMockData = [][]interface{}{
		{2, 0, "o362ksNHl8gIL4cbXjkMEQ", "pfp8ZcSwhg15W2YSaooX8g", 1546591119847974875, 1, addrA, `{}`},
		{7, 0, "CKI1kAfqOxWpmUug23OxTQ", "iTbk_EvsiprSwLLpC9LOgg", 1546591304102924848, 1, addrA, `{}`},
		{7, 1, "nLwnh4a9oiOG9n4FtgboRw", "iTbk_EvsiprSwLLpC9LOgg", 1546591304284859585, 4, addrB, `{}`},
		{7, 2, "mrsmkMHz1mcXwsOJDakLxA", "iTbk_EvsiprSwLLpC9LOgg", 1546591304583827173, 2, addrB, `{}`},
		{7, 3, "YrJ64M2odTb96B4VHIWCMw", "iTbk_EvsiprSwLLpC9LOgg", 1546591304847472713, 2, addrA, `{}`},
		{7, 4, "7iCSm4vy4FvAapGCT2p9MA", "iTbk_EvsiprSwLLpC9LOgg", 1546591304901837474, 1, addrB, `{}`},
		{10, 0, "U1s0IRuyLd3iw8PdlAKv4A", "er05e7FvAZOP3gP5_w_RKw", 1546591421847471717, 1, addrA, `{}`},
		{10, 1, "5MX357EQDlMUxZVPjjXeFQ", "er05e7FvAZOP3gP5_w_RKw", 1546591421791893744, 4, addrB, `{}`},
		{10, 2, "lXTWT_P7NRxMHukZCEUfng", "er05e7FvAZOP3gP5_w_RKw", 1546591421909181774, 2, addrB, `{}`},
	}
)

func mockData(t *testing.T) {
	db, err := models.OpenSQLiteDBAsGorp(testdb, "rw", 5, 2)
	if err != nil {
		t.Errorf("open testdb failed")
		return
	}
	defer db.Db.Close()

	// create tables
	for _, ddlSQL := range ddls {
		if i, err := db.Exec(ddlSQL); err != nil {
			t.Errorf("execute ddl #%d failed: %v", i, err)
		}
	}

	var insertRows = func(writeSQL string, data [][]interface{}) error {
		for i, row := range data {
			if _, err := db.Exec(writeSQL, row...); err != nil {
				return errors.Wrapf(err, "write row #%d failed", i)
			}
		}
		return nil
	}

	if err := insertRows(
		"insert into indexed_blocks values (?,?,?,?,?,?,?,?)",
		blocksMockData,
	); err != nil {
		t.Errorf("mock data for indexed_blocks failed: %v", err)
	}

	if err := insertRows(
		"insert into indexed_transactions values (?,?,?,?,?,?,?,?)",
		transactionsMockData,
	); err != nil {
		t.Errorf("mock data for indexed_transactions failed: %v", err)
	}
}

func setupWebsocketClient(addr string) (client *jsonrpc2.Conn, err error) {
	// TODO: dial timeout
	conn, _, err := websocket.DefaultDialer.DialContext(
		context.Background(),
		addr,
		nil,
	)
	if err != nil {
		return nil, err
	}

	var connOpts []jsonrpc2.ConnOpt
	return jsonrpc2.NewConn(
		context.Background(),
		wsstream.NewObjectStream(conn),
		nil,
		connOpts...,
	), nil
}

type bpGetBlockTestCase struct {
	Height         int
	Hash           string
	ExpectedResult []interface{}
}

func (c *bpGetBlockTestCase) String() string {
	return fmt.Sprintf("fetch block of height %d hashed %q", c.Height, c.Hash)
}

type bpGetTransactionListTestCase struct {
	Since           string
	Direction       string
	Limit           int
	ExpectedResults [][]interface{}
}

func (c *bpGetTransactionListTestCase) Params() interface{} {
	return []interface{}{c.Since, c.Direction, c.Limit}
}

func (c *bpGetTransactionListTestCase) String() string {
	return fmt.Sprintf("fetch %d transactions %s since %s", c.Limit, c.Direction, c.Since)
}

type bpGetTransactionByHashTestCase struct {
	Hash           string
	ExpectedResult []interface{}
}

func (c *bpGetTransactionByHashTestCase) String() string {
	return fmt.Sprintf("fetch transaction hashed %q", c.Hash)
}

func TestService(t *testing.T) {
	t.Logf("testdb: %s", testdb)
	mockData(t)
	defer os.Remove(testdb + "-shm")
	defer os.Remove(testdb + "-wal")
	defer os.Remove(testdb)

	port := 8546
	// log.SetLevel(log.DebugLevel)
	service := api.NewService()
	service.DBFile = testdb
	service.WebsocketAddr = ":" + strconv.Itoa(port)
	service.StartServers()
	defer service.StopServersAndWait()

	var (
		addr     = fmt.Sprintf("ws://localhost:%d", port)
		callOpts []jsonrpc2.CallOption

		conveyBlock = func(convey C, item *models.Block, cp []interface{}) {
			if cp == nil {
				convey.So(item, ShouldBeNil)
				return
			}
			convey.So(item.Height, ShouldEqual, cp[0].(int))
			convey.So(item.Hash, ShouldEqual, cp[1].(string))
			convey.So(item.Timestamp, ShouldEqual, cp[2].(int))
			convey.So(item.TimestampHuman.UnixNano(), ShouldEqual, item.Timestamp)
			convey.So(item.Version, ShouldEqual, cp[3].(int))
			convey.So(item.Producer, ShouldEqual, cp[4].(string))
			convey.So(item.MerkleRoot, ShouldEqual, cp[5].(string))
			convey.So(item.Parent, ShouldEqual, cp[6].(string))
			convey.So(item.TxCount, ShouldEqual, cp[7].(int))
		}

		conveyTransaction = func(convey C, item *models.Transaction, cp []interface{}) {
			if cp == nil {
				convey.So(item, ShouldBeNil)
				return
			}

			convey.So(item.BlockHeight, ShouldEqual, cp[0].(int))
			convey.So(item.TxIndex, ShouldEqual, cp[1].(int))
			convey.So(item.Hash, ShouldEqual, cp[2].(string))
			convey.So(item.BlockHash, ShouldEqual, cp[3].(string))
			convey.So(item.Timestamp, ShouldEqual, cp[4].(int))
			convey.So(item.TimestampHuman.UnixNano(), ShouldEqual, item.Timestamp)
			convey.So(item.TxType, ShouldEqual, cp[5].(int))
			convey.So(item.Address, ShouldEqual, cp[6].(string))
			convey.So(item.Raw, ShouldEqual, cp[7].(string))
		}
	)

	Convey("blocks API", t, func() {
		rpc, err := setupWebsocketClient(addr)
		if err != nil {
			t.Errorf("failed to connect to wsapi server: %v", err)
			return
		}

		Convey("bp_getBlockList should fail on invalid parameters", func() {
			var (
				result    []*models.Block
				testCases = map[string][]int{
					"to-from < 5":   {1, 5},
					"to-from > 100": {1, 102},
				}
			)

			for name, testCase := range testCases {
				Convey(name, func() {
					err := rpc.Call(context.Background(), "bp_getBlockList", testCase, &result, callOpts...)
					So(err, ShouldNotBeNil)
				})
			}

		})

		Convey("bp_getBlockList should success on fetching valid number of blocks", func() {
			var (
				result    []*models.Block
				testCases = [][]int{
					{1, 6},
					{1, 11},
					{2, 9},
				}
			)

			for i, testCase := range testCases {
				from, to := testCase[0], testCase[1]
				count := to - from
				name := fmt.Sprintf("case#%d, fetch %d blocks [%d, %d)", i, count, from, to)
				Convey(name, func(c C) {

					err := rpc.Call(context.Background(), "bp_getBlockList", testCase, &result, callOpts...)
					So(err, ShouldBeNil)
					So(len(result), ShouldEqual, count)
					for i, item := range result {
						cp := blocksMockData[count+from-2-i]
						conveyBlock(c, item, cp)
					}
				})
			}
		})

		Convey("bp_getBlockByHash should fetch blocks on existed hash and nothing for an non-existed hash", func(c C) {
			var (
				result = new(models.Block)

				testCases = []*bpGetBlockTestCase{
					{0, "o362ksNHl8gIL4cbXjkMEQ", nil},
					{0, "HGGcDJqO7tuZWwJyFxRl9g", blocksMockData[0]},
				}
			)

			for i, testCase := range testCases {
				Convey(fmt.Sprintf("case#%d: %s", i, testCase.String()), func() {
					err := rpc.Call(
						context.Background(),
						"bp_getBlockByHash",
						[]interface{}{testCase.Hash},
						&result,
						callOpts...,
					)
					So(err, ShouldBeNil)
					conveyBlock(c, result, testCase.ExpectedResult)
				})
			}
		})

		Convey("bp_getBlockByHeight should fetch blocks on existed height and nothing for an non-existed height", func(c C) {
			var (
				result = new(models.Block)

				testCases = []*bpGetBlockTestCase{
					{192124141, "", nil},
					{1, "", blocksMockData[0]},
				}
			)

			for i, testCase := range testCases {
				Convey(fmt.Sprintf("case#%d: %s", i, testCase.String()), func() {
					err := rpc.Call(
						context.Background(),
						"bp_getBlockByHeight",
						[]interface{}{testCase.Height},
						&result,
						callOpts...,
					)
					So(err, ShouldBeNil)
					conveyBlock(c, result, testCase.ExpectedResult)
				})
			}
		})

		Reset(func() {
			// teardown
			rpc.Close()
		})
	})

	Convey("transactions API", t, func() {
		rpc, err := setupWebsocketClient(addr)
		if err != nil {
			t.Errorf("failed to connect to wsapi server: %v", err)
			return
		}

		Convey("bp_getTransactionList should fail on invalid parameters", func() {
			var (
				result                []*models.Transaction
				invalidParameterCases = map[string][]interface{}{
					"limit < 5":         {"nLwnh4a9oiOG9n4FtgboRw", "backward", 4},
					"limit > 100":       {"nLwnh4a9oiOG9n4FtgboRw", "backward", 101},
					"unknown direction": {"nLwnh4a9oiOG9n4FtgboRw", "unknown", 10},
				}
			)

			for name, testCase := range invalidParameterCases {
				Convey(name, func() {
					err := rpc.Call(
						context.Background(),
						"bp_getTransactionList",
						testCase,
						&result,
						callOpts...,
					)
					So(err, ShouldNotBeNil)
				})
			}
		})

		Convey("bp_getTransactionList should success on fetching valid number of transactions", func(c C) {
			var (
				result    []*models.Transaction
				testCases = []bpGetTransactionListTestCase{
					{"5MX357EQDlMUxZVPjjXeFQ", "backward", 5, transactionsMockData[2:7]},
					{"CKI1kAfqOxWpmUug23OxTQ", "backward", 5, transactionsMockData[0:1]},
					{"CKI1kAfqOxWpmUug23OxTQ", "forward", 7, transactionsMockData[2:9]},
				}
			)

			for i, testCase := range testCases {
				Convey(fmt.Sprintf("case#%d: %s", i, testCase.String()), func() {
					err := rpc.Call(
						context.Background(),
						"bp_getTransactionList",
						testCase.Params(),
						&result,
						callOpts...,
					)
					So(err, ShouldBeNil)
					So(len(result), ShouldEqual, len(testCase.ExpectedResults))
					for i, item := range result {
						cp := testCase.ExpectedResults[i]
						if testCase.Direction == "backward" {
							cp = testCase.ExpectedResults[len(result)-i-1]
						}
						conveyTransaction(c, item, cp)
					}
				})
			}
		})

		Convey("bp_getTransactionByHash should fetch transactions on existed hash and nothing for an non-existed hash", func(c C) {
			var (
				result = new(models.Transaction)

				testCases = []*bpGetTransactionByHashTestCase{
					{"o362ksNHl8gIL4cbXjkMEQ", transactionsMockData[0]},
					{"HGGcDJqO7tuZWwJyFxRl9g", nil},
				}
			)

			for i, testCase := range testCases {
				Convey(fmt.Sprintf("case#%d: %s", i, testCase.String()), func() {
					err := rpc.Call(
						context.Background(),
						"bp_getTransactionByHash",
						[]interface{}{testCase.Hash},
						&result,
						callOpts...,
					)
					So(err, ShouldBeNil)
					conveyTransaction(c, result, testCase.ExpectedResult)
				})
			}
		})

		Reset(func() {
			rpc.Close()
		})
	})
}
