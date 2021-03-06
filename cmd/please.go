// Copyright © 2018 Salim Alami Idrissi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/celrenheit/alfred/assets"
	"github.com/celrenheit/alfred/parser"
	"github.com/celrenheit/alfred/wallet"
	"github.com/manifoldco/promptui"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stellar/go/build"
	"github.com/stellar/go/clients/horizon"
	"github.com/stellar/go/keypair"
)

// pleaseCmd represents the import command
var pleaseCmd = &cobra.Command{
	Use:     "please",
	Short:   "Run commands such as send and buy",
	Aliases: []string{"p"},
	Long:    `please command allows to execute command`,
	Example: `alfred please send 20 XLM from master to jennifer
alfred please send 33 MOBI from master to jennifer

alfred please buy 100 MOBI using XLM (will pick the best price)
alfred please buy MOBI using 100 XLM (will pick the best price)
alfred please buy 100 MOBI AT 0.1000 using XLM
alfred please sell 100 MOBI FOR XLM (will pick the best price)
	`,
	PreRunE: middlewares(checkDB, checkSecret),
	Run: func(cmd *cobra.Command, args []string) {
		var query string
		switch {
		case len(args) == 1:
			query = args[0]
		case len(args) > 1:
			query = strings.Join(args, " ")
		}

		statement, err := parser.Parse(query)
		if err != nil {
			fatal(err)
		}

		client := getClient(viper.GetBool("testnet"))

		path := viper.GetString("db")
		secret := viper.GetString("secret")
		m, err := wallet.OpenSecretString(path, secret)
		if err != nil {
			fatal(err)
		}

		switch req := statement.(type) {
		case *parser.SendRequest:
			err = sendRequest(m, client, cmd, req)
		case *parser.ShareAccountRequest:
			err = shareRequest(m, client, cmd, req)
		case *parser.SetDataRequest:
			err = setData(m, client, cmd, req)
		case *parser.Offer:
			err = createOffer(m, client, cmd, req)
		default:
			fatalf("unsupported statement type: %T", statement.Kind())
		}

		if err != nil {
			fatalf(describeHorizonError(err))
		}
	},
}

func describeHorizonError(err error) string {
	if err == nil {
		return ""
	}

	e, ok := err.(*horizon.Error)
	if !ok {
		return err.Error()
	}
	pb := e.Problem
	return fmt.Sprintf("%s (%s)", pb.Title, string(pb.Extras["result_codes"]))
}

func init() {
	RootCmd.AddCommand(pleaseCmd)

	pleaseCmd.Flags().BoolP("yes", "y", false, "if set, no confirmation prompt will be shown")
	viper.BindPFlags(pleaseCmd.Flags())
}

func sendRequest(m *wallet.Alfred, client *horizon.Client, cmd *cobra.Command, req *parser.SendRequest) error {
	// Check choosen currency
	asset, err := selectAsset(req.Currency)
	if err != nil {
		return err
	}

	// Check trust

	var (
		from = req.From
		to   = req.To
	)

	src, err := getOrSelectWallet(m, from)
	if err != nil {
		return err
	}

	var memo build.TransactionMutator
	if to != "" {
		if addr, err := keypair.Parse(to); err == nil { // to custom address
			to = addr.Address()
		} else {
			if w := m.WalletByName(to); w != nil { // between wallet
				to = w.Keypair.Address()
			} else if contact, ok := m.Stellar.Contacts[to]; ok { // to contact
				to = contact.Address
				if contact.Memo != nil {
					memo = contact.Memo.ToTransactionMutator()
				}
			} else {
				return fmt.Errorf("destination '%s' not found", to)
			}
		}
	} else {
		var toList []string
		for name, _ := range m.Stellar.Contacts {
			toList = append(toList, name)
		}
		prompt := promptui.SelectWithAdd{
			Label:    "Destination",
			AddLabel: "Another address",
			Items:    toList,
			Validate: func(input string) error {
				_, err := keypair.Parse(input)
				return err
			},
		}

		_, name, err := prompt.Run()
		if err != nil {
			return err
		}

		contact := m.Stellar.Contacts[name]
		to = contact.Address
		if contact.Memo != nil {
			memo = contact.Memo.ToTransactionMutator()
		}
	}

	srcAcc, exists, err := getAccount(client, src.Address())
	if err != nil {
		return err
	}

	if !exists {
		return fmt.Errorf("source account does exists, please fund it first")
	}

	destAcc, exists, err := getAccount(client, to)
	if err != nil {
		return err
	}

	if !hasTrustline(destAcc, *asset) {
		return fmt.Errorf("destination account needs to trust %v", asset)
	}

	var amount interface{}
	if asset.BuilderAsset.Native {
		amount = build.NativeAmount{Amount: req.Amount}
	} else {
		amount = build.CreditAmount{
			Code:   asset.BuilderAsset.Code,
			Issuer: asset.BuilderAsset.Issuer,
			Amount: req.Amount,
		}
	}

	var txnMutator build.TransactionMutator
	if exists {
		txnMutator = build.Payment(
			build.Destination{AddressOrSeed: to},
			amount,
		)
	} else {
		txnMutator = build.CreateAccount(
			build.Destination{AddressOrSeed: to},
			amount,
		)
	}

	opts := []build.TransactionMutator{
		build.SourceAccount{src.Seed()},
		build.AutoSequence{SequenceProvider: client},
		txnMutator,
	}
	if memo != nil {
		opts = append(opts, memo)
	}

	if !hasTrustline(srcAcc, *asset) {
		opts = append(opts, build.Trust(asset.BuilderAsset.Code, asset.BuilderAsset.Issuer))
	}

	if viper.GetBool("testnet") {
		opts = append(opts, build.TestNetwork)
	} else {
		opts = append(opts, build.PublicNetwork)
	}

	tx, err := build.Transaction(opts...)
	if err != nil {
		return err
	}

	txe, err := tx.Sign(src.Seed())
	if err != nil {
		return err
	}

	txeB64, err := txe.Base64()
	if err != nil {
		return err
	}

	if !viper.GetBool("yes") {
		printSummaryTable(map[string]string{
			"Amount":      req.Amount,
			"Currency":    req.Currency,
			"Source":      src.Address(),
			"Destination": to,
		})

		_, err = (&promptui.Prompt{
			Label:     "Are you sure",
			IsConfirm: true,
		}).Run()
		if err != nil {
			return err
		}
	}

	resp, err := client.SubmitTransaction(txeB64)
	if err != nil {
		return err
	}

	fmt.Println(resp.Hash)
	return nil
}

func shareRequest(m *wallet.Alfred, client *horizon.Client, cmd *cobra.Command, req *parser.ShareAccountRequest) error {
	getAddress := func(in string) keypair.KP {
		if kp, err := keypair.Parse(in); err == nil { // to custom address
			return kp
		} else {
			if w := m.WalletByName(in); w != nil { // between wallet
				return w.Keypair
			} else if contact, ok := m.Stellar.Contacts[in]; ok { // to contact
				return keypair.MustParse(contact.Address)
			}
		}
		return nil
	}

	addr := getAddress(req.Account)
	if addr == nil {
		return fmt.Errorf("'%v' wallet not found", req.Account)
	}

	src := addr.(*keypair.Full)

	masterAcc, exists, err := getAccount(client, addr.Address())
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("'%v' does not exist, fund it first", req.Account)
	}

	var newSigners []horizon.Account
	for _, name := range req.AdditionnalSigners {
		addr := getAddress(name)
		if addr == nil {
			return fmt.Errorf("address not found for '%v'", name)
		}

		acc, exists, err := getAccount(client, addr.Address())
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("'%v' does not exist, fund it first", name)
		}

		newSigners = append(newSigners, acc)
	}

	threshold := uint32(1 + len(req.AdditionnalSigners) + len(masterAcc.Signers))

	var sopts []interface{}
	for _, acc := range newSigners {
		sopts = append(sopts, build.AddSigner(acc.AccountID, 1))
	}
	sopts = append(sopts,
		build.MasterWeight(threshold),
		build.SetThresholds(1, 1, threshold),
	)

	opts := []build.TransactionMutator{
		build.SourceAccount{src.Seed()},
		build.AutoSequence{SequenceProvider: client},
		build.SetOptions(sopts...),
	}

	if viper.GetBool("testnet") {
		opts = append(opts, build.TestNetwork)
	} else {
		opts = append(opts, build.PublicNetwork)
	}

	tx, err := build.Transaction(opts...)
	if err != nil {
		return err
	}

	txe, err := tx.Sign(src.Seed())
	if err != nil {
		return err
	}

	txeB64, err := txe.Base64()
	if err != nil {
		return err
	}

	if !viper.GetBool("yes") {
		_, err = (&promptui.Prompt{
			Label:     "Are you sure",
			IsConfirm: true,
		}).Run()
		if err != nil {
			return err
		}
	}

	resp, err := client.SubmitTransaction(txeB64)
	if err != nil {
		return err
	}

	fmt.Println(resp.Hash)
	return nil
}

func setData(m *wallet.Alfred, client *horizon.Client, cmd *cobra.Command, req *parser.SetDataRequest) error {
	src, err := selectWallet(m)
	if err != nil {
		return err
	}

	var sopts []build.TransactionMutator
	for key, value := range req.KVs {
		var data []byte
		switch value.Kind {
		case parser.SetDataFromString:
			data = []byte(value.Value)
		case parser.SetDataFromFile:
			data, err = ioutil.ReadFile(value.Value)

			if err != nil {
				return err
			}
		}

		sopts = append(sopts, build.SetData(key, data))
	}

	opts := []build.TransactionMutator{
		build.SourceAccount{src.Seed()},
		build.AutoSequence{SequenceProvider: client},
	}

	opts = append(opts, sopts...)

	if viper.GetBool("testnet") {
		opts = append(opts, build.TestNetwork)
	} else {
		opts = append(opts, build.PublicNetwork)
	}

	tx, err := build.Transaction(opts...)
	if err != nil {
		return err
	}

	txe, err := tx.Sign(src.Seed())
	if err != nil {
		return err
	}

	txeB64, err := txe.Base64()
	if err != nil {
		return err
	}

	if !viper.GetBool("yes") {
		_, err = (&promptui.Prompt{
			Label:     "Are you sure",
			IsConfirm: true,
		}).Run()
		if err != nil {
			return err
		}
	}

	resp, err := client.SubmitTransaction(txeB64)
	if err != nil {
		return err
	}

	fmt.Println(resp.Hash)
	return nil
}

func createOffer(m *wallet.Alfred, client *horizon.Client, cmd *cobra.Command, req *parser.Offer) error {
	src, err := getOrSelectWallet(m, req.Account)
	if err != nil {
		return err
	}

	buying, err := selectAsset(req.Buying)
	if err != nil {
		return err
	}

	selling, err := selectAsset(req.Selling)
	if err != nil {
		return err
	}

	amount, err := strconv.ParseFloat(req.Amount, 64)
	if err != nil {
		return err
	}

	price := req.Price
	if price == "" {
		var (
			book      horizon.OrderBookSummary
			priceLvls []horizon.PriceLevel
		)

		book, err = client.LoadOrderBook(selling.ToHorizonAsset(), buying.ToHorizonAsset())
		if err != nil {
			return err
		}

		switch req.Kind() {
		case parser.BuyOfferKind:
			priceLvls = book.Bids
		case parser.SellOfferKind:
			priceLvls = book.Bids
		}

		if len(priceLvls) == 0 {
			return errors.New("no offers found in the orderbook, you should specify a price")
		}

		price = priceLvls[0].Price

		p, err := strconv.ParseFloat(price, 64)
		if err != nil {
			return err
		}
		switch req.Kind() {
		case parser.BuyOfferKind:
			switch req.AmountKind {
			case parser.AmountBuyKind:
				amount /= p
			case parser.AmountSellKind:

			}
		case parser.SellOfferKind:
		}
	}

	strAmount := strconv.FormatFloat(amount, 'f', 7, 64)
	var amountDescr string
	switch req.Kind() {
	case parser.BuyOfferKind:
		amountDescr = fmt.Sprintf("%s %s = %f %s", req.Amount, buying.CodeString(), amount, selling.CodeString())
	case parser.SellOfferKind:
		amountDescr = fmt.Sprintf("%f %s", amount, selling.CodeString())
	}

	opts := []build.TransactionMutator{
		build.SourceAccount{src.Seed()},
		build.AutoSequence{SequenceProvider: client},
		build.CreateOffer(build.Rate{
			Buying:  buying.BuilderAsset,
			Selling: selling.BuilderAsset,
			Price:   build.Price(price),
		}, build.Amount(strAmount)),
	}

	if viper.GetBool("testnet") {
		opts = append(opts, build.TestNetwork)
	} else {
		opts = append(opts, build.PublicNetwork)
	}

	tx, err := build.Transaction(opts...)
	if err != nil {
		return err
	}

	txe, err := tx.Sign(src.Seed())
	if err != nil {
		return err
	}

	txeB64, err := txe.Base64()
	if err != nil {
		return err
	}

	if !viper.GetBool("yes") {
		printSummaryTable(map[string]string{
			"Amount":  amountDescr,
			"Buying":  buying.String(),
			"Selling": selling.String(),
			"Price":   price,
		})
		_, err = (&promptui.Prompt{
			Label:     "Are you sure",
			IsConfirm: true,
		}).Run()
		if err != nil {
			return err
		}
	}

	resp, err := client.SubmitTransaction(txeB64)
	if err != nil {
		return err
	}

	fmt.Println(resp.Hash)
	return nil
}

func hasTrustline(acc horizon.Account, asset assets.Asset) bool {
	if asset.BuilderAsset.Native {
		return true
	}
	for _, b := range acc.Balances {
		if b.Code == asset.BuilderAsset.Code && b.Issuer == asset.BuilderAsset.Issuer {
			return true
		}
	}

	return false
}

func getAddress(m *wallet.Alfred, in string) keypair.KP {
	if kp, err := keypair.Parse(in); err == nil { // to custom address
		return kp
	} else {
		if w := m.WalletByName(in); w != nil { // between wallet
			return w.Keypair
		} else if contact, ok := m.Stellar.Contacts[in]; ok { // to contact
			return keypair.MustParse(contact.Address)
		}
	}
	return nil
}

func selectWallet(m *wallet.Alfred) (*keypair.Full, error) {
	sel := promptui.Select{
		Label: "Select Wallet",
		Items: m.Stellar.Wallets,
	}

	idx, _, err := sel.Run()
	if err != nil {
		return nil, err
	}

	return m.Stellar.Wallets[idx].Keypair.(*keypair.Full), nil
}

func selectAsset(cur string) (*assets.Asset, error) {
	if strings.ToLower(cur) == "lumens" {
		cur = "XLM"
	}

	asts := assets.GetAssets(cur)
	if len(asts) == 0 {
		return nil, fmt.Errorf("asset %v is not supported right now", cur)
	}

	var asset assets.Asset
	if len(asts) == 1 { // only one we check this
		asset = asts[0]
	} else { // otherwise, prompt
		idx, _, err := (&promptui.Select{
			Label: "Choose currency",
			Items: asts,
		}).Run()
		if err != nil {
			return nil, err
		}

		asset = asts[idx]
	}

	return &asset, nil
}

func getOrSelectWallet(m *wallet.Alfred, from string) (src *keypair.Full, err error) {
	if from != "" {
		var w *wallet.Wallet
		if addr, err := keypair.Parse(from); err == nil {
			w = m.WalletByAddress(addr.Address())
		} else {
			w = m.WalletByName(from)
		}

		if w == nil {
			return nil, fmt.Errorf("wallet '%s' not found", from)
		}

		src = w.Keypair.(*keypair.Full)
	} else {
		src, err = selectWallet(m)
		if err != nil {
			return nil, err
		}
	}

	return src, nil
}

func printSummaryTable(kvs map[string]string) {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetAlignment(tablewriter.ALIGN_RIGHT)

	network := "PUBLIC"
	if viper.GetBool("testnet") {
		network = "TESTNET"
	}

	kvs["Network"] = network
	for k, v := range kvs {
		table.Append([]string{k, v})
	}
	table.Render()
}
