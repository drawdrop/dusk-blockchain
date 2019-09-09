package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	ristretto "github.com/bwesterb/go-ristretto"
	cfg "github.com/dusk-network/dusk-blockchain/pkg/config"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/initiator"
	"github.com/dusk-network/dusk-blockchain/pkg/core/consensus/maintainer"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database"
	"github.com/dusk-network/dusk-blockchain/pkg/core/database/heavy"
	"github.com/dusk-network/dusk-blockchain/pkg/core/transactor"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire"
	"github.com/dusk-network/dusk-blockchain/pkg/p2p/wire/topics"
	wallet "github.com/dusk-network/dusk-blockchain/pkg/wallet"
	walletdb "github.com/dusk-network/dusk-blockchain/pkg/wallet/database"
	"github.com/dusk-network/dusk-crypto/mlsag"
	"github.com/dusk-network/dusk-wallet/key"
	zkproof "github.com/dusk-network/dusk-zkproof"

	"github.com/dusk-network/dusk-blockchain/pkg/wallet/transactions"
)

var testnet = byte(2)

// TODO: rename
type CLI struct {
	eventBroker wire.EventBroker
	rpcBus      *wire.RPCBus
	transactor  *transactor.Transactor
}

func (c *CLI) createWalletCMD(args []string) {
	if c.transactor != nil {
		fmt.Fprintln(os.Stdout, "You have already loaded a wallet. Please re-start the node to load another.")
	}

	if args == nil || len(args) < 1 {
		fmt.Fprintf(os.Stdout, commandInfo["createwallet"]+"\n")
		return
	}
	password := args[0]

	db, err := walletdb.New(cfg.Get().Wallet.Store)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error opening database: %v\n", err)
		return
	}

	w, err := wallet.New(rand.Read, testnet, db, fetchDecoys, fetchInputs, password)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error creating wallet: %v\n", err)
		return
	}
	pubAddr, err := w.PublicAddress()
	if err != nil {
		fmt.Fprintf(os.Stdout, "error attempting to get your public address: %v\n", err)
		return
	}

	fmt.Fprintf(os.Stdout, "Wallet created successfully!\n")
	fmt.Fprintf(os.Stdout, "Public Address: %s\n", pubAddr)

	c.transactor = transactor.New(w, nil)

	if !cfg.Get().General.WalletOnly {
		go initiator.LaunchConsensus(c.eventBroker, c.rpcBus, w)
		if err := c.launchMaintainer(w); err != nil {
			fmt.Fprintf(os.Stdout, "could not launch maintainer - consensus transactions will not be automated: %v\n", err)
		}
	}
}

func (c *CLI) launchMaintainer(w *wallet.Wallet) error {
	r := cfg.Get()
	value := r.Consensus.DefaultValue
	lockTime := r.Consensus.DefaultLockTime
	buffer := r.Consensus.DefaultBuffer
	k, err := w.ReconstructK()
	if err != nil {
		return err
	}

	return maintainer.Launch(c.eventBroker, nil, w.ConsensusKeys().BLSPubKeyBytes, zkproof.CalculateM(k), c.transactor, value, lockTime, buffer)
}

func (c *CLI) loadWalletCMD(args []string) {
	if c.transactor != nil {
		fmt.Fprintln(os.Stdout, "You have already loaded a wallet. Please re-start the node to load another.")
	}

	if args == nil || len(args) < 1 {
		fmt.Fprintf(os.Stdout, commandInfo["loadwallet"]+"\n")
		return
	}
	password := args[0]

	w, err := loadWallet(password)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error attempting to load wallet: %v\n", err)
		return
	}

	pubAddr, err := w.PublicAddress()
	if err != nil {
		fmt.Fprintf(os.Stdout, "error attempting to get your public address: %v\n", err)
		return
	}

	fmt.Fprintf(os.Stdout, "Wallet loaded successfully!\n")
	fmt.Fprintf(os.Stdout, "Public Address: %s\n", pubAddr)

	c.transactor = transactor.New(w, nil)

	if !cfg.Get().General.WalletOnly {
		go initiator.LaunchConsensus(c.eventBroker, c.rpcBus, w)
		if err := c.launchMaintainer(w); err != nil {
			fmt.Fprintf(os.Stdout, "could not launch maintainer - consensus transactions will not be automated: %v\n", err)
		}
	}
}

func loadWallet(password string) (*wallet.Wallet, error) {
	// First load the database
	db, err := walletdb.New(cfg.Get().Wallet.Store)
	if err != nil {
		db.Close()
		return nil, err
	}

	// Then load the wallet
	w, err := wallet.LoadFromFile(testnet, db, fetchDecoys, fetchInputs, password)
	if err != nil {
		db.Close()
		return nil, err
	}

	return w, nil
}

func (c *CLI) createFromSeedCMD(args []string) {
	if c.transactor != nil {
		fmt.Fprintln(os.Stdout, "You have already loaded a wallet. Please re-start the node to load another.")
	}

	if args == nil || len(args) < 2 {
		fmt.Fprintf(os.Stdout, commandInfo["createfromseed"]+"\n")
		return
	}

	seed := args[0]
	password := args[1]
	seedBytes, err := hex.DecodeString(seed)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error attempting to decode seed: %v\n", err)
		return
	}

	// Then load the wallet
	w, err := createFromSeed(seedBytes, password)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error attempting to create wallet from seed: %v\n", err)
		return
	}

	pubAddr, err := w.PublicAddress()
	if err != nil {
		fmt.Fprintf(os.Stdout, "error attempting to get your public address: %v\n", err)
		return
	}

	fmt.Fprintf(os.Stdout, "Wallet loaded successfully!\nPublic Address: %s\n", pubAddr)

	c.transactor = transactor.New(w, nil)

	if !cfg.Get().General.WalletOnly {
		go initiator.LaunchConsensus(c.eventBroker, c.rpcBus, w)
		if err := c.launchMaintainer(w); err != nil {
			fmt.Fprintf(os.Stdout, "could not launch maintainer - consensus transactions will not be automated: %v\n", err)
		}
	}
}

func createFromSeed(seedBytes []byte, password string) (*wallet.Wallet, error) {
	// First load the database
	db, err := walletdb.New(cfg.Get().Wallet.Store)
	if err != nil {
		return nil, err
	}

	// Then load the wallet
	w, err := wallet.LoadFromSeed(seedBytes, testnet, db, fetchDecoys, fetchInputs, password)
	if err != nil {
		return nil, err
	}

	return w, nil
}

func (c *CLI) balanceCMD() {
	if c.transactor == nil {
		fmt.Fprintln(os.Stdout, "Please load a wallet before checking your balance")
		return
	}

	balance, err := c.transactor.Balance()
	if err != nil {
		fmt.Fprintf(os.Stdout, "error retrieving balance: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stdout, balance)
}

func (c *CLI) transferCMD(args []string) {
	if c.transactor == nil {
		fmt.Fprintln(os.Stdout, "Please load a wallet before sending DUSK")
		return
	}

	if len(args) < 2 {
		fmt.Fprintln(os.Stdout, "Please specify an amount and an address")
		return
	}

	amount, err := stringToUint64(args[0])
	if err != nil {
		fmt.Fprintf(os.Stdout, "error converting amount string to an integer: %v\n", err)
		return
	}

	tx, err := c.transactor.CreateStandardTx(amount, args[1])
	if err != nil {
		fmt.Fprintf(os.Stdout, "error creating transaction: %v\n", err)
		return
	}

	buf := new(bytes.Buffer)
	if err := transactions.Marshal(buf, tx); err != nil {
		fmt.Fprintf(os.Stdout, "error encoding transaction: %v\n", err)
	}

	c.eventBroker.Publish(string(topics.Tx), buf)
}

func (c *CLI) sendBidCMD(args []string) {
	if c.transactor == nil {
		fmt.Fprintln(os.Stdout, "Please load a wallet before bidding DUSK")
		return
	}

	amount, err := stringToUint64(args[0])
	if err != nil {
		fmt.Fprintf(os.Stdout, "error converting amount string to an integer: %v\n", err)
		return
	}

	lockTime, err := stringToUint64(args[1])
	if err != nil {
		fmt.Fprintf(os.Stdout, "error converting locktime string to an integer: %v\n", err)
		return
	}

	tx, err := c.transactor.CreateBidTx(amount, lockTime)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error creating transaction: %v\n", err)
		return
	}

	buf := new(bytes.Buffer)
	if err := transactions.Marshal(buf, tx); err != nil {
		fmt.Fprintf(os.Stdout, "error encoding transaction: %v\n", err)
	}

	c.eventBroker.Publish(string(topics.Tx), buf)
}

func (c *CLI) sendStakeCMD(args []string) {
	if c.transactor == nil {
		fmt.Fprintln(os.Stdout, "Please load a wallet before staking DUSK")
		return
	}

	amount, err := stringToUint64(args[0])
	if err != nil {
		fmt.Fprintf(os.Stdout, "error converting amount string to an integer: %v\n", err)
		return
	}

	lockTime, err := stringToUint64(args[1])
	if err != nil {
		fmt.Fprintf(os.Stdout, "error converting locktime string to an integer: %v\n", err)
		return
	}

	tx, err := c.transactor.CreateStakeTx(amount, lockTime)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error creating transaction: %v\n", err)
		return
	}

	buf := new(bytes.Buffer)
	if err := transactions.Marshal(buf, tx); err != nil {
		fmt.Fprintf(os.Stdout, "error encoding transaction: %v\n", err)
	}

	c.eventBroker.Publish(string(topics.Tx), buf)
}

func fetchDecoys(numMixins int) []mlsag.PubKeys {
	_, db := heavy.CreateDBConnection()

	var pubKeys []mlsag.PubKeys
	var decoys []ristretto.Point
	db.View(func(t database.Transaction) error {
		decoys = t.FetchDecoys(numMixins)
		return nil
	})

	// Potential panic if the database does not have enough decoys
	for i := 0; i < numMixins; i++ {

		var keyVector mlsag.PubKeys
		keyVector.AddPubKey(decoys[i])

		var secondaryKey ristretto.Point
		secondaryKey.Rand()
		keyVector.AddPubKey(secondaryKey)

		pubKeys = append(pubKeys, keyVector)
	}
	return pubKeys
}

func generateDualKey() mlsag.PubKeys {
	pubkeys := mlsag.PubKeys{}

	for i := 0; i < 2; i++ {
		var key ristretto.Point
		key.Rand()
		pubkeys.AddPubKey(key)
	}

	return pubkeys
}

func fetchInputs(netPrefix byte, db *walletdb.DB, totalAmount int64, key *key.Key) ([]*transactions.Input, int64, error) {
	// Fetch all inputs from database that are >= totalAmount
	// returns error if inputs do not add up to total amount
	privSpend, err := key.PrivateSpend()
	if err != nil {
		return nil, 0, err
	}
	return db.FetchInputs(privSpend.Bytes(), totalAmount)
}

func (c *CLI) setLocktimeCMD(args []string) {
	if args == nil || len(args) < 1 {
		fmt.Fprintln(os.Stdout, "please specify a default locktime")
		return
	}

	locktime, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintln(os.Stdout, err)
		return
	}

	if locktime > 250000 {
		fmt.Fprintln(os.Stdout, "locktime can not be higher than 250000")
	}

	// Send value to tx sender
	fmt.Println(locktime)
}

func (c *CLI) setDefaultValueCMD(args []string) {
	if args == nil || len(args) < 1 {
		fmt.Fprintln(os.Stdout, "please specify a default value")
		return
	}

	value, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintln(os.Stdout, err)
		return
	}

	// Send value to tx sender
	fmt.Println(value)
}
