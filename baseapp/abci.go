package baseapp

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"syscall"

	abci "github.com/tendermint/tendermint/abci/types"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/snapshots"
	store "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

// InitChain implements the ABCI interface. It runs the initialization logic
// directly on the CommitMultiStore.
func (app *BaseApp) InitChain(req abci.RequestInitChain) (res abci.ResponseInitChain) {
	// stash the consensus params in the cms main store and memoize
	if req.ConsensusParams != nil {
		app.setConsensusParams(req.ConsensusParams)
		app.storeConsensusParams(req.ConsensusParams)
	}

	initHeader := abci.Header{ChainID: req.ChainId, Time: req.Time}

	// initialize the deliver state and check state with a correct header
	app.setDeliverState(initHeader)
	app.setCheckState(initHeader)

	if app.initChainer == nil {
		return
	}

	// add block gas meter for any genesis transactions (allow infinite gas)
	app.deliverState.ctx = app.deliverState.ctx.WithBlockGasMeter(sdk.NewInfiniteGasMeter())

	res = app.initChainer(app.deliverState.ctx, req)

	// sanity check
	if len(req.Validators) > 0 {
		if len(req.Validators) != len(res.Validators) {
			panic(
				fmt.Errorf(
					"len(RequestInitChain.Validators) != len(GenesisValidators) (%d != %d)",
					len(req.Validators), len(res.Validators),
				),
			)
		}

		sort.Sort(abci.ValidatorUpdates(req.Validators))
		sort.Sort(abci.ValidatorUpdates(res.Validators))

		for i, val := range res.Validators {
			if !val.Equal(req.Validators[i]) {
				panic(fmt.Errorf("genesisValidators[%d] != req.Validators[%d] ", i, i))
			}
		}
	}

	// NOTE: We don't commit, but BeginBlock for block 1 starts from this
	// deliverState.
	return res
}

// Info implements the ABCI interface.
func (app *BaseApp) Info(req abci.RequestInfo) abci.ResponseInfo {
	lastCommitID := app.cms.LastCommitID()

	return abci.ResponseInfo{
		Data:             app.name,
		LastBlockHeight:  lastCommitID.Version,
		LastBlockAppHash: lastCommitID.Hash,
	}
}

// SetOption implements the ABCI interface.
func (app *BaseApp) SetOption(req abci.RequestSetOption) (res abci.ResponseSetOption) {
	// TODO: Implement!
	return
}

// FilterPeerByAddrPort filters peers by address/port.
func (app *BaseApp) FilterPeerByAddrPort(info string) abci.ResponseQuery {
	if app.addrPeerFilter != nil {
		return app.addrPeerFilter(info)
	}
	return abci.ResponseQuery{}
}

// FilterPeerByIDfilters peers by node ID.
func (app *BaseApp) FilterPeerByID(info string) abci.ResponseQuery {
	if app.idPeerFilter != nil {
		return app.idPeerFilter(info)
	}
	return abci.ResponseQuery{}
}

// BeginBlock implements the ABCI application interface.
func (app *BaseApp) BeginBlock(req abci.RequestBeginBlock) (res abci.ResponseBeginBlock) {
	if app.cms.TracingEnabled() {
		app.cms.SetTracingContext(sdk.TraceContext(
			map[string]interface{}{"blockHeight": req.Header.Height},
		))
	}

	if err := app.validateHeight(req); err != nil {
		panic(err)
	}

	// Initialize the DeliverTx state. If this is the first block, it should
	// already be initialized in InitChain. Otherwise app.deliverState will be
	// nil, since it is reset on Commit.
	if app.deliverState == nil {
		app.setDeliverState(req.Header)
	} else {
		// In the first block, app.deliverState.ctx will already be initialized
		// by InitChain. Context is now updated with Header information.
		app.deliverState.ctx = app.deliverState.ctx.
			WithBlockHeader(req.Header).
			WithBlockHeight(req.Header.Height)
	}

	// add block gas meter
	var gasMeter sdk.GasMeter
	if maxGas := app.getMaximumBlockGas(); maxGas > 0 {
		gasMeter = sdk.NewGasMeter(maxGas)
	} else {
		gasMeter = sdk.NewInfiniteGasMeter()
	}

	app.deliverState.ctx = app.deliverState.ctx.WithBlockGasMeter(gasMeter)

	if app.beginBlocker != nil {
		res = app.beginBlocker(app.deliverState.ctx, req)
	}

	// set the signed validators for addition to context in deliverTx
	app.voteInfos = req.LastCommitInfo.GetVotes()
	return res
}

// EndBlock implements the ABCI interface.
func (app *BaseApp) EndBlock(req abci.RequestEndBlock) (res abci.ResponseEndBlock) {
	if app.deliverState.ms.TracingEnabled() {
		app.deliverState.ms = app.deliverState.ms.SetTracingContext(nil).(sdk.CacheMultiStore)
	}

	if app.endBlocker != nil {
		res = app.endBlocker(app.deliverState.ctx, req)
	}

	return
}

// CheckTx implements the ABCI interface and executes a tx in CheckTx mode. In
// CheckTx mode, messages are not executed. This means messages are only validated
// and only the AnteHandler is executed. State is persisted to the BaseApp's
// internal CheckTx state if the AnteHandler passes. Otherwise, the ResponseCheckTx
// will contain releveant error information. Regardless of tx execution outcome,
// the ResponseCheckTx will contain relevant gas execution context.
func (app *BaseApp) CheckTx(req abci.RequestCheckTx) abci.ResponseCheckTx {
	tx, err := app.txDecoder(req.Tx)
	if err != nil {
		return sdkerrors.ResponseCheckTx(err, 0, 0)
	}

	var mode runTxMode

	switch {
	case req.Type == abci.CheckTxType_New:
		mode = runTxModeCheck

	case req.Type == abci.CheckTxType_Recheck:
		mode = runTxModeReCheck

	default:
		panic(fmt.Sprintf("unknown RequestCheckTx type: %s", req.Type))
	}

	gInfo, result, err := app.runTx(mode, req.Tx, tx)
	if err != nil {
		return sdkerrors.ResponseCheckTx(err, gInfo.GasWanted, gInfo.GasUsed)
	}

	return abci.ResponseCheckTx{
		GasWanted: int64(gInfo.GasWanted), // TODO: Should type accept unsigned ints?
		GasUsed:   int64(gInfo.GasUsed),   // TODO: Should type accept unsigned ints?
		Log:       result.Log,
		Data:      result.Data,
		Events:    result.Events,
	}
}

// DeliverTx implements the ABCI interface and executes a tx in DeliverTx mode.
// State only gets persisted if all messages are valid and get executed successfully.
// Otherwise, the ResponseDeliverTx will contain releveant error information.
// Regardless of tx execution outcome, the ResponseDeliverTx will contain relevant
// gas execution context.
func (app *BaseApp) DeliverTx(req abci.RequestDeliverTx) abci.ResponseDeliverTx {
	tx, err := app.txDecoder(req.Tx)
	if err != nil {
		return sdkerrors.ResponseDeliverTx(err, 0, 0)
	}

	gInfo, result, err := app.runTx(runTxModeDeliver, req.Tx, tx)
	if err != nil {
		return sdkerrors.ResponseDeliverTx(err, gInfo.GasWanted, gInfo.GasUsed)
	}

	return abci.ResponseDeliverTx{
		GasWanted: int64(gInfo.GasWanted), // TODO: Should type accept unsigned ints?
		GasUsed:   int64(gInfo.GasUsed),   // TODO: Should type accept unsigned ints?
		Log:       result.Log,
		Data:      result.Data,
		Events:    result.Events,
	}
}

// Commit implements the ABCI interface. It will commit all state that exists in
// the deliver state's multi-store and includes the resulting commit ID in the
// returned abci.ResponseCommit. Commit will set the check state based on the
// latest header and reset the deliver state. Also, if a non-zero halt height is
// defined in config, Commit will execute a deferred function call to check
// against that height and gracefully halt if it matches the latest committed
// height.
func (app *BaseApp) Commit() (res abci.ResponseCommit) {
	header := app.deliverState.ctx.BlockHeader()

	// Write the DeliverTx state which is cache-wrapped and commit the MultiStore.
	// The write to the DeliverTx state writes all state transitions to the root
	// MultiStore (app.cms) so when Commit() is called is persists those values.
	app.deliverState.ms.Write()
	commitID := app.cms.Commit()
	app.logger.Debug("Commit synced", "commit", fmt.Sprintf("%X", commitID))

	// Reset the Check state to the latest committed.
	//
	// NOTE: This is safe because Tendermint holds a lock on the mempool for
	// Commit. Use the header from this latest block.
	app.setCheckState(header)

	// empty/reset the deliver state
	app.deliverState = nil

	var halt bool

	switch {
	case app.haltHeight > 0 && uint64(header.Height) >= app.haltHeight:
		halt = true

	case app.haltTime > 0 && header.Time.Unix() >= int64(app.haltTime):
		halt = true
	}

	if halt {
		// Halt the binary and allow Tendermint to receive the ResponseCommit
		// response with the commit ID hash. This will allow the node to successfully
		// restart and process blocks assuming the halt configuration has been
		// reset or moved to a more distant value.
		app.halt()
	}

	if app.snapshotInterval > 0 && uint64(header.Height)%app.snapshotInterval == 0 {
		go app.snapshot(uint64(header.Height))
	}

	return abci.ResponseCommit{
		Data: commitID.Hash,
	}
}

// halt attempts to gracefully shutdown the node via SIGINT and SIGTERM falling
// back on os.Exit if both fail.
func (app *BaseApp) halt() {
	app.logger.Info("halting node per configuration", "height", app.haltHeight, "time", app.haltTime)

	p, err := os.FindProcess(os.Getpid())
	if err == nil {
		// attempt cascading signals in case SIGINT fails (os dependent)
		sigIntErr := p.Signal(syscall.SIGINT)
		sigTermErr := p.Signal(syscall.SIGTERM)

		if sigIntErr == nil || sigTermErr == nil {
			return
		}
	}

	// Resort to exiting immediately if the process could not be found or killed
	// via SIGINT/SIGTERM signals.
	app.logger.Info("failed to send SIGINT/SIGTERM; exiting...")
	os.Exit(0)
}

// snapshot takes a snapshot of the current state and prunes any old snapshots
func (app *BaseApp) snapshot(height uint64) {
	format := store.SnapshotFormat
	app.logger.Info("Taking state snapshot", "height", height, "format", format)
	if app.snapshotStore == nil {
		app.logger.Error("No snapshot store configured")
		return
	}
	if app.snapshotStore.Active() {
		app.logger.Error("A state snapshot is already in progress")
		return
	}
	chunks, err := app.cms.Snapshot(height, format)
	if err != nil {
		app.logger.Error("Failed to take state snapshot", "height", height, "format", format,
			"err", err.Error())
		return
	}
	err = app.snapshotStore.Save(height, format, chunks)
	if err != nil {
		app.logger.Error("Failed to take state snapshot", "height", height, "format", format,
			"err", err.Error())
		return
	}
	app.logger.Info("Completed state snapshot", "height", height, "format", format)

	if app.snapshotRetention > 0 {
		app.logger.Debug("Pruning state snapshots")
		pruned, err := app.snapshotStore.Prune(app.snapshotRetention)
		if err != nil {
			app.logger.Error("Failed to prune state snapshots", "err", err.Error())
			return
		}
		app.logger.Debug("Pruned state snapshots", "pruned", pruned)
	}
}

// Query implements the ABCI interface. It delegates to CommitMultiStore if it
// implements Queryable.
func (app *BaseApp) Query(req abci.RequestQuery) abci.ResponseQuery {
	path := splitPath(req.Path)
	if len(path) == 0 {
		sdkerrors.QueryResult(sdkerrors.Wrap(sdkerrors.ErrUnknownRequest, "no query path provided"))
	}

	switch path[0] {
	// "/app" prefix for special application queries
	case "app":
		return handleQueryApp(app, path, req)

	case "store":
		return handleQueryStore(app, path, req)

	case "p2p":
		return handleQueryP2P(app, path)

	case "custom":
		return handleQueryCustom(app, path, req)
	}

	return sdkerrors.QueryResult(sdkerrors.Wrap(sdkerrors.ErrUnknownRequest, "unknown query path"))
}

// ListSnapshots implements the ABCI interface. It delegates to app.snapshotStore if set.
func (app *BaseApp) ListSnapshots(req abci.RequestListSnapshots) abci.ResponseListSnapshots {
	resp := abci.ResponseListSnapshots{
		Snapshots: []*abci.Snapshot{},
	}
	if app.snapshotStore == nil {
		return resp
	}

	snapshots, err := app.snapshotStore.List()
	if err != nil {
		app.logger.Error("Failed to list snapshots", "err", err.Error())
		return resp
	}
	for _, snapshot := range snapshots {
		hashes := make([][]byte, 0, len(snapshot.Chunks))
		for _, chunk := range snapshot.Chunks {
			hashes = append(hashes, chunk.Hash)
		}
		resp.Snapshots = append(resp.Snapshots, &abci.Snapshot{
			Height:      snapshot.Height,
			Format:      snapshot.Format,
			ChunkHashes: hashes,
			Metadata:    nil,
		})
	}

	return resp
}

// LoadSnapshotChunk implements the ABCI interface. It delegates to app.snapshotStore if set.
func (app *BaseApp) LoadSnapshotChunk(req abci.RequestLoadSnapshotChunk) abci.ResponseLoadSnapshotChunk {
	resp := abci.ResponseLoadSnapshotChunk{}
	if app.snapshotStore == nil {
		return resp
	}

	chunk, err := app.snapshotStore.LoadChunk(req.Height, req.Format, req.Chunk)
	if err != nil {
		app.logger.Error("Failed to load snapshot chunk", "height", req.Height, "format", req.Format,
			"chunk", req.Chunk, "err", err.Error())
		return resp
	}
	if chunk == nil {
		return resp
	}
	defer chunk.Close()

	resp.Chunk, err = ioutil.ReadAll(chunk)
	if err != nil {
		app.logger.Error("Failed to load snapshot chunk contents", "height", req.Height,
			"format", req.Format, "chunk", req.Chunk, "err", err.Error())
		return resp
	}

	return resp
}

// OfferSnapshot implements the ABCI interface. It delegates to app.snapshotStore if set.
func (app *BaseApp) OfferSnapshot(req abci.RequestOfferSnapshot) abci.ResponseOfferSnapshot {
	if req.Snapshot == nil {
		app.logger.Error("Received nil snapshot")
		return abci.ResponseOfferSnapshot{Reason: abci.ResponseOfferSnapshot_internal_error}
	}
	if app.snapshotRestorer != nil {
		app.logger.Error("Snapshot restoration already in progress")
		return abci.ResponseOfferSnapshot{Reason: abci.ResponseOfferSnapshot_internal_error}
	}
	if req.Snapshot.Format != store.SnapshotFormat {
		app.logger.Info("Unsupported snapshot format", "format", req.Snapshot.Format)
		return abci.ResponseOfferSnapshot{Reason: abci.ResponseOfferSnapshot_invalid_format}
	}

	snapshot := snapshots.Snapshot{
		Height: req.Snapshot.Height,
		Format: req.Snapshot.Format,
		Chunks: make([]*snapshots.Chunk, 0, len(req.Snapshot.ChunkHashes)),
	}
	for _, hash := range req.Snapshot.ChunkHashes {
		snapshot.Chunks = append(snapshot.Chunks, &snapshots.Chunk{Hash: hash})
	}
	restorer, err := snapshots.NewRestorer(snapshot, app.cms)
	if err != nil {
		app.logger.Error("Snapshot restoration failed", "height", req.Snapshot.Height,
			"format", req.Snapshot.Format, "error", err.Error())
		return abci.ResponseOfferSnapshot{Reason: abci.ResponseOfferSnapshot_internal_error}
	}
	app.snapshotRestorer = restorer

	return abci.ResponseOfferSnapshot{Accepted: true}
}

// ApplySnapshotChunk implements the ABCI interface. It delegates to app.snapshotStore if set.
// The checksum should already have been checked by the caller.
func (app *BaseApp) ApplySnapshotChunk(req abci.RequestApplySnapshotChunk) abci.ResponseApplySnapshotChunk {
	snapshot := app.snapshotRestorer.Snapshot()
	if snapshot == nil {
		app.logger.Error("No snapshot in progress")
		return abci.ResponseApplySnapshotChunk{Reason: abci.ResponseApplySnapshotChunk_internal_error}
	}
	done, err := app.snapshotRestorer.Add(ioutil.NopCloser(bytes.NewReader(req.Chunk)))
	if err != nil {
		app.logger.Error("Failed to restore snapshot", "height", snapshot.Height,
			"format", snapshot.Format, "error", err.Error())
		app.snapshotRestorer.Close()
		return abci.ResponseApplySnapshotChunk{Reason: abci.ResponseApplySnapshotChunk_internal_error}
	}
	if done {
		app.snapshotRestorer.Close()
		app.snapshotRestorer = nil
	}
	return abci.ResponseApplySnapshotChunk{Applied: true}
}

func handleQueryApp(app *BaseApp, path []string, req abci.RequestQuery) abci.ResponseQuery {
	if len(path) >= 2 {
		switch path[1] {
		case "simulate":
			txBytes := req.Data

			tx, err := app.txDecoder(txBytes)
			if err != nil {
				return sdkerrors.QueryResult(sdkerrors.Wrap(err, "failed to decode tx"))
			}

			gInfo, res, err := app.Simulate(txBytes, tx)
			if err != nil {
				return sdkerrors.QueryResult(sdkerrors.Wrap(err, "failed to simulate tx"))
			}

			simRes := &sdk.SimulationResponse{
				GasInfo: gInfo,
				Result:  res,
			}

			bz, err := codec.ProtoMarshalJSON(simRes)
			if err != nil {
				return sdkerrors.QueryResult(sdkerrors.Wrap(err, "failed to JSON encode simulation response"))
			}

			return abci.ResponseQuery{
				Codespace: sdkerrors.RootCodespace,
				Height:    req.Height,
				Value:     bz,
			}

		case "version":
			return abci.ResponseQuery{
				Codespace: sdkerrors.RootCodespace,
				Height:    req.Height,
				Value:     []byte(app.appVersion),
			}

		default:
			return sdkerrors.QueryResult(sdkerrors.Wrapf(sdkerrors.ErrUnknownRequest, "unknown query: %s", path))
		}
	}

	return sdkerrors.QueryResult(
		sdkerrors.Wrap(
			sdkerrors.ErrUnknownRequest,
			"expected second parameter to be either 'simulate' or 'version', neither was present",
		),
	)
}

func handleQueryStore(app *BaseApp, path []string, req abci.RequestQuery) abci.ResponseQuery {
	// "/store" prefix for store queries
	queryable, ok := app.cms.(sdk.Queryable)
	if !ok {
		return sdkerrors.QueryResult(sdkerrors.Wrap(sdkerrors.ErrUnknownRequest, "multistore doesn't support queries"))
	}

	req.Path = "/" + strings.Join(path[1:], "/")

	// when a client did not provide a query height, manually inject the latest
	if req.Height == 0 {
		req.Height = app.LastBlockHeight()
	}

	if req.Height <= 1 && req.Prove {
		return sdkerrors.QueryResult(
			sdkerrors.Wrap(
				sdkerrors.ErrInvalidRequest,
				"cannot query with proof when height <= 1; please provide a valid height",
			),
		)
	}

	resp := queryable.Query(req)
	resp.Height = req.Height

	return resp
}

func handleQueryP2P(app *BaseApp, path []string) abci.ResponseQuery {
	// "/p2p" prefix for p2p queries
	if len(path) >= 4 {
		cmd, typ, arg := path[1], path[2], path[3]
		switch cmd {
		case "filter":
			switch typ {
			case "addr":
				return app.FilterPeerByAddrPort(arg)

			case "id":
				return app.FilterPeerByID(arg)
			}

		default:
			return sdkerrors.QueryResult(sdkerrors.Wrap(sdkerrors.ErrUnknownRequest, "expected second parameter to be 'filter'"))
		}
	}

	return sdkerrors.QueryResult(
		sdkerrors.Wrap(
			sdkerrors.ErrUnknownRequest, "expected path is p2p filter <addr|id> <parameter>",
		),
	)
}

func handleQueryCustom(app *BaseApp, path []string, req abci.RequestQuery) abci.ResponseQuery {
	// path[0] should be "custom" because "/custom" prefix is required for keeper
	// queries.
	//
	// The QueryRouter routes using path[1]. For example, in the path
	// "custom/gov/proposal", QueryRouter routes using "gov".
	if len(path) < 2 || path[1] == "" {
		return sdkerrors.QueryResult(sdkerrors.Wrap(sdkerrors.ErrUnknownRequest, "no route for custom query specified"))
	}

	querier := app.queryRouter.Route(path[1])
	if querier == nil {
		return sdkerrors.QueryResult(sdkerrors.Wrapf(sdkerrors.ErrUnknownRequest, "no custom querier found for route %s", path[1]))
	}

	// when a client did not provide a query height, manually inject the latest
	if req.Height == 0 {
		req.Height = app.LastBlockHeight()
	}

	if req.Height <= 1 && req.Prove {
		return sdkerrors.QueryResult(
			sdkerrors.Wrap(
				sdkerrors.ErrInvalidRequest,
				"cannot query with proof when height <= 1; please provide a valid height",
			),
		)
	}

	cacheMS, err := app.cms.CacheMultiStoreWithVersion(req.Height)
	if err != nil {
		return sdkerrors.QueryResult(
			sdkerrors.Wrapf(
				sdkerrors.ErrInvalidRequest,
				"failed to load state at height %d; %s (latest height: %d)", req.Height, err, app.LastBlockHeight(),
			),
		)
	}

	// cache wrap the commit-multistore for safety
	ctx := sdk.NewContext(
		cacheMS, app.checkState.ctx.BlockHeader(), true, app.logger,
	).WithMinGasPrices(app.minGasPrices)

	// Passes the rest of the path as an argument to the querier.
	//
	// For example, in the path "custom/gov/proposal/test", the gov querier gets
	// []string{"proposal", "test"} as the path.
	resBytes, err := querier(ctx, path[2:], req)
	if err != nil {
		space, code, log := sdkerrors.ABCIInfo(err, false)
		return abci.ResponseQuery{
			Code:      code,
			Codespace: space,
			Log:       log,
			Height:    req.Height,
		}
	}

	return abci.ResponseQuery{
		Height: req.Height,
		Value:  resBytes,
	}
}

// splitPath splits a string path using the delimiter '/'.
//
// e.g. "this/is/funny" becomes []string{"this", "is", "funny"}
func splitPath(requestPath string) (path []string) {
	path = strings.Split(requestPath, "/")

	// first element is empty string
	if len(path) > 0 && path[0] == "" {
		path = path[1:]
	}

	return path
}
