package lib

import (
	"context"
	"errors"
	"fmt"
	"strings"

	clinode "github.com/filecoin-project/boost/cli/node"
	"github.com/filecoin-project/boost/markets/piecestore"
	piecestoreimpl "github.com/filecoin-project/boost/markets/piecestore/impl"
	"github.com/filecoin-project/boost/storagemarket/types/legacytypes"
	vfsm "github.com/filecoin-project/go-ds-versioning/pkg/fsm"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-statemachine/fsm"
	"github.com/filecoin-project/lotus/api"
	lapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/messagesigner"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/backupds"
	"github.com/filecoin-project/lotus/node/modules"
	"github.com/filecoin-project/lotus/node/repo"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	ds_sync "github.com/ipfs/go-datastore/sync"
	"github.com/manifoldco/promptui"
	"github.com/urfave/cli/v2"
)

func OpenDataStore(path string) (*backupds.Datastore, error) {
	ctx := context.Background()

	rpo, err := repo.NewFS(path)
	if err != nil {
		return nil, fmt.Errorf("could not open repo %s: %w", path, err)
	}

	exists, err := rpo.Exists()
	if err != nil {
		return nil, fmt.Errorf("checking repo %s exists: %w", path, err)
	}
	if !exists {
		return nil, fmt.Errorf("repo does not exist: %s", path)
	}

	lr, err := rpo.Lock(repo.StorageMiner)
	if err != nil {
		return nil, fmt.Errorf("locking repo %s: %w", path, err)
	}

	mds, err := lr.Datastore(ctx, "/metadata")
	if err != nil {
		return nil, err
	}

	bds, err := backupds.Wrap(mds, "")
	if err != nil {
		return nil, fmt.Errorf("opening backupds: %w", err)
	}

	return bds, nil
}

func GetPropCidByChainDealID(ctx context.Context, ds *backupds.Datastore) (map[abi.DealID]cid.Cid, error) {
	deals, err := getLegacyDealsFSM(ctx, ds)
	if err != nil {
		return nil, err
	}

	// Build a mapping of chain deal ID to proposal CID
	var list []legacytypes.MinerDeal
	if err := deals.List(&list); err != nil {
		return nil, err
	}

	byChainDealID := make(map[abi.DealID]cid.Cid, len(list))
	for _, d := range list {
		if d.DealID != 0 {
			byChainDealID[d.DealID] = d.ProposalCid
		}
	}

	return byChainDealID, nil
}

func OpenPieceStore(ctx context.Context, ds *backupds.Datastore) (piecestore.PieceStore, error) {
	// Open the piece store
	ps, err := piecestoreimpl.NewPieceStore(namespace.Wrap(ds, datastore.NewKey("/storagemarket")))
	if err != nil {
		return nil, fmt.Errorf("creating piece store from datastore : %w", err)
	}

	// Wait for the piece store to be ready
	ch := make(chan error, 1)
	ps.OnReady(func(e error) {
		ch <- e
	})

	err = ps.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting piece store: %w", err)
	}

	select {
	case err = <-ch:
		if err != nil {
			return nil, fmt.Errorf("waiting for piece store to be ready: %w", err)
		}
	case <-ctx.Done():
		return nil, errors.New("cancelled while waiting for piece store to be ready")
	}

	return ps, nil
}

func getLegacyDealsFSM(ctx context.Context, ds *backupds.Datastore) (fsm.Group, error) {
	// Get the deals FSM
	provDS := namespace.Wrap(ds, datastore.NewKey("/deals/provider"))
	deals, migrate, err := vfsm.NewVersionedFSM(provDS, fsm.Parameters{
		StateType:     legacytypes.MinerDeal{},
		StateKeyField: "State",
	}, nil, "2")
	if err != nil {
		return nil, fmt.Errorf("reading legacy deals from datastore: %w", err)
	}

	err = migrate(ctx)
	if err != nil {
		return nil, fmt.Errorf("running provider fsm migration script: %w", err)
	}

	return deals, err
}

// SignAndPushToMpoolWithGas signs a message with custom gas parameters and pushes it to the message pool.
// Parameters:
//   - maxFee: maximum fee in attoFIL the sender is willing to pay (optional)
//   - gasLimit: gas limit for the message (optional, will be estimated if not set)
//   - gasFeeCap: gas fee cap in attoFIL per gas unit (optional)
//   - gasPremium: gas premium in attoFIL per gas unit, higher values will make transactions mine faster (optional)
//
// Returns the message CID, whether it was sent, and any error that occurred.
func SignAndPushToMpoolWithGas(cctx *cli.Context, ctx context.Context, api api.Gateway, n *clinode.Node, ds *ds_sync.MutexDatastore, msg *types.Message, maxFee *big.Int, gasLimit int64, gasFeeCap, gasPremium *big.Int) (cid cid.Cid, sent bool, err error) {
	if ds == nil {
		ds = ds_sync.MutexWrap(datastore.NewMapDatastore())
	}
	vmessagesigner := messagesigner.NewMessageSigner(n.Wallet, &modules.MpoolNonceAPI{ChainModule: api, StateModule: api}, ds)

	head, err := api.ChainHead(ctx)
	if err != nil {
		return
	}
	basefee := head.Blocks()[0].ParentBaseFee

	// Apply custom gas parameters if provided
	spec := &lapi.MessageSendSpec{}
	if maxFee != nil {
		spec.MaxFee = *maxFee
	} else {
		spec.MaxFee = abi.NewTokenAmount(1000000000) // 1 nFIL default
	}

	// Apply gas parameters before estimation if provided
	if gasLimit > 0 {
		msg.GasLimit = gasLimit
	}
	if gasFeeCap != nil {
		msg.GasFeeCap = *gasFeeCap
	}
	if gasPremium != nil {
		msg.GasPremium = *gasPremium
	}

	// Only estimate if gasLimit is not set
	if msg.GasLimit == 0 {
		msg, err = api.GasEstimateMessageGas(ctx, msg, spec, types.EmptyTSK)
		if err != nil {
			err = fmt.Errorf("GasEstimateMessageGas error: %w", err)
			return
		}
	}

	// If gasFeeCap is not set, use basefee + 20%
	if gasFeeCap == nil {
		newGasFeeCap := big.Mul(big.Int(basefee), big.NewInt(6))
		newGasFeeCap = big.Div(newGasFeeCap, big.NewInt(5))

		if big.Cmp(msg.GasFeeCap, newGasFeeCap) < 0 {
			msg.GasFeeCap = newGasFeeCap
		}
	}

	smsg, err := vmessagesigner.SignMessage(ctx, msg, nil, func(*types.SignedMessage) error { return nil })
	if err != nil {
		return
	}

	fmt.Println("about to send message with the following gas costs")
	maxFeeCalc := big.Mul(smsg.Message.GasFeeCap, big.NewInt(smsg.Message.GasLimit))
	fmt.Println("max fee:     ", types.FIL(maxFeeCalc), "(absolute maximum amount you are willing to pay to get your transaction confirmed)")
	fmt.Println("gas fee cap: ", types.FIL(smsg.Message.GasFeeCap))
	fmt.Println("gas limit:   ", smsg.Message.GasLimit)
	fmt.Println("gas premium: ", types.FIL(smsg.Message.GasPremium))
	fmt.Println("basefee:     ", types.FIL(basefee))
	fmt.Println("nonce:       ", smsg.Message.Nonce)
	fmt.Println()
	if !cctx.Bool("assume-yes") {
		validate := func(input string) error {
			if strings.EqualFold(input, "y") || strings.EqualFold(input, "yes") {
				return nil
			}
			if strings.EqualFold(input, "n") || strings.EqualFold(input, "no") {
				return nil
			}
			return errors.New("incorrect input")
		}

		templates := &promptui.PromptTemplates{
			Prompt:  "{{ . }} ",
			Valid:   "{{ . | green }} ",
			Invalid: "{{ . | red }} ",
			Success: "{{ . | cyan | bold }} ",
		}

		prompt := promptui.Prompt{
			Label:     "Proceed? Yes [Y/y] / No [N/n], Ctrl+C (^C) to exit",
			Templates: templates,
			Validate:  validate,
		}

		var input string

		input, err = prompt.Run()
		if err != nil {
			return
		}
		if strings.Contains(strings.ToLower(input), "n") {
			fmt.Println("Message not sent")
			return
		}
	}

	cid, err = api.MpoolPush(ctx, smsg)
	if err != nil {
		err = fmt.Errorf("mpool push: failed to push message: %w", err)
		return
	}
	fmt.Println("sent message: ", cid)
	sent = true
	return
}

func SignAndPushToMpool(cctx *cli.Context, ctx context.Context, api api.Gateway, n *clinode.Node, ds *ds_sync.MutexDatastore, msg *types.Message) (cid cid.Cid, sent bool, err error) {
	if ds == nil {
		ds = ds_sync.MutexWrap(datastore.NewMapDatastore())
	}
	vmessagesigner := messagesigner.NewMessageSigner(n.Wallet, &modules.MpoolNonceAPI{ChainModule: api, StateModule: api}, ds)

	head, err := api.ChainHead(ctx)
	if err != nil {
		return
	}
	basefee := head.Blocks()[0].ParentBaseFee

	spec := &lapi.MessageSendSpec{
		MaxFee: abi.NewTokenAmount(1000000000), // 1 nFIL
	}
	msg, err = api.GasEstimateMessageGas(ctx, msg, spec, types.EmptyTSK)
	if err != nil {
		err = fmt.Errorf("GasEstimateMessageGas error: %w", err)
		return
	}

	// use basefee + 20%
	newGasFeeCap := big.Mul(big.Int(basefee), big.NewInt(6))
	newGasFeeCap = big.Div(newGasFeeCap, big.NewInt(5))

	if big.Cmp(msg.GasFeeCap, newGasFeeCap) < 0 {
		msg.GasFeeCap = newGasFeeCap
	}

	smsg, err := vmessagesigner.SignMessage(ctx, msg, nil, func(*types.SignedMessage) error { return nil })
	if err != nil {
		return
	}

	fmt.Println("about to send message with the following gas costs")
	maxFee := big.Mul(smsg.Message.GasFeeCap, big.NewInt(smsg.Message.GasLimit))
	fmt.Println("max fee:     ", types.FIL(maxFee), "(absolute maximum amount you are willing to pay to get your transaction confirmed)")
	fmt.Println("gas fee cap: ", types.FIL(smsg.Message.GasFeeCap))
	fmt.Println("gas limit:   ", smsg.Message.GasLimit)
	fmt.Println("gas premium: ", types.FIL(smsg.Message.GasPremium))
	fmt.Println("basefee:     ", types.FIL(basefee))
	fmt.Println("nonce:       ", smsg.Message.Nonce)
	fmt.Println()
	if !cctx.Bool("assume-yes") {
		validate := func(input string) error {
			if strings.EqualFold(input, "y") || strings.EqualFold(input, "yes") {
				return nil
			}
			if strings.EqualFold(input, "n") || strings.EqualFold(input, "no") {
				return nil
			}
			return errors.New("incorrect input")
		}

		templates := &promptui.PromptTemplates{
			Prompt:  "{{ . }} ",
			Valid:   "{{ . | green }} ",
			Invalid: "{{ . | red }} ",
			Success: "{{ . | cyan | bold }} ",
		}

		prompt := promptui.Prompt{
			Label:     "Proceed? Yes [Y/y] / No [N/n], Ctrl+C (^C) to exit",
			Templates: templates,
			Validate:  validate,
		}

		var input string

		input, err = prompt.Run()
		if err != nil {
			return
		}
		if strings.Contains(strings.ToLower(input), "n") {
			fmt.Println("Message not sent")
			return
		}
	}

	cid, err = api.MpoolPush(ctx, smsg)
	if err != nil {
		err = fmt.Errorf("mpool push: failed to push message: %w", err)
		return
	}
	fmt.Println("sent message: ", cid)
	sent = true
	return
}
