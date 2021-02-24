// Package main implements the ledger backed oasis-node signer plugin.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	pluginSigner "github.com/oasisprotocol/oasis-core/go/common/crypto/signature/signers/plugin"

	"github.com/oasisprotocol/oasis-core-ledger/cmd"
	"github.com/oasisprotocol/oasis-core-ledger/common"
	"github.com/oasisprotocol/oasis-core-ledger/common/wallet"
	"github.com/oasisprotocol/oasis-core-ledger/internal"
)

var (
	// signerPathCoinType is set to 474, the number associated with Oasis ROSE.
	signerPathCoinType uint32 = 474
	// signerPathAccount is the account index used to sign transactions.
	signerPathAccount uint32 = 0
	// SignerPathChange indicates an external chain.
	signerPathChange uint32 = 0

	// signerEntityDerivationRootPath is the BIP-0032 path prefix used for generating
	// an Entity signer.
	signerEntityDerivationRootPath = []uint32{
		internal.PathPurposeBIP44,
		signerPathCoinType,
		signerPathAccount,
		signerPathChange,
	}
	// signerConsensusDerivationRootPath is the derivation path prefix used for
	// generating a consensus signer.
	signerConsensusDerivationRootPath = []uint32{
		internal.PathPurposeConsensus,
		signerPathCoinType,
		internal.PathSubPurposeConsensus,
		signerPathAccount,
	}

	roleDerivationRootPaths = map[signature.SignerRole][]uint32{
		signature.SignerEntity:    signerEntityDerivationRootPath,
		signature.SignerConsensus: signerConsensusDerivationRootPath,
	}

	rootCmd = &cobra.Command{
		Use:   "ledger-signer",
		Short: "Oasis Core Ledger Signer",
		Args:  cobra.NoArgs,
		Run:   func(*cobra.Command, []string) {},
		Long: `Oasis Core Ledger Signer

This binary is the Oasis Core Ledger Signer plugin.
It is not meant to be executed directly.
You need to execute oasis-node from Oasis Core with appropriate flags and it
will load this plugin automatically.
`,
		Version: common.SoftwareVersion,
	}
)

type pluginConfig struct {
	walletID *wallet.ID
	index    uint32
}

func newPluginConfig(cfgStr string) (*pluginConfig, error) {
	var kvStrs []string

	// Don't try to split cfgStr if no configuration is specified.
	if cfgStr != "" {
		kvStrs = strings.Split(cfgStr, ",")
	}

	var (
		cfg                       pluginConfig
		foundWalletID, foundIndex bool
	)
	for _, v := range kvStrs {
		spl := strings.Split(v, ":")
		if len(spl) != 2 {
			return nil, fmt.Errorf("malformed k/v pair: '%s'", v)
		}

		key := strings.ToLower(spl[0])
		switch key {
		case "wallet_id":
			if foundWalletID {
				return nil, fmt.Errorf("wallet ID already configured")
			}
			cfg.walletID = new(wallet.ID)
			if err := cfg.walletID.UnmarshalHex(spl[1]); err != nil {
				return nil, err
			}
			foundWalletID = true
		case "index":
			if foundIndex {
				return nil, fmt.Errorf("index already configured")
			}
			idx, err := strconv.ParseUint(spl[1], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("malformed index: %w", err)
			}
			cfg.index = uint32(idx)
			foundIndex = true
		default:
			return nil, fmt.Errorf("unknown configuration option: '%v'", spl[0])
		}
	}

	return &cfg, nil
}

type ledgerPlugin struct {
	walletID *wallet.ID
	inner    map[signature.SignerRole]*ledgerSigner
}

type ledgerSigner struct {
	path []uint32

	device    *internal.LedgerOasis
	publicKey *signature.PublicKey
}

func (pl *ledgerPlugin) Initialize(config string, roles ...signature.SignerRole) error {
	cfg, err := newPluginConfig(config)
	if err != nil {
		return fmt.Errorf("ledger: failed to parse configuration: %w", err)
	}
	pl.walletID = cfg.walletID
	pl.inner = make(map[signature.SignerRole]*ledgerSigner)

	for _, role := range roles {
		var signer ledgerSigner
		pathPrefix, ok := roleDerivationRootPaths[role]
		if !ok {
			return fmt.Errorf("ledger: role %d is not supported by signer", role)
		}
		signer.path = append(signer.path, pathPrefix...)
		signer.path = append(signer.path, cfg.index)

		pl.inner[role] = &signer
	}

	return nil
}

func (pl *ledgerPlugin) Load(role signature.SignerRole, _mustGenerate bool) error {
	// Note: `mustGenerate` is ignored as all keys are generated on the
	// Ledger device.

	signer, device, err := pl.signerForRole(role)
	if err != nil {
		return err
	}
	if device != nil {
		// Already connected to device with this key's path.
		return nil
	}

	dev, err := internal.ConnectApp(pl.walletID, internal.ListingDerivationPath)
	if err != nil {
		return fmt.Errorf("ledger: failed to connect to device: %w", err)
	}
	signer.device = dev

	return nil
}

func (pl *ledgerPlugin) Public(role signature.SignerRole) (signature.PublicKey, error) {
	var pubKey signature.PublicKey

	signer, device, err := pl.signerForRole(role)
	if err != nil {
		return pubKey, err
	}
	if signer.publicKey != nil {
		// Already have retrieved the public key.
		return *signer.publicKey, nil
	}
	if device == nil {
		return pubKey, fmt.Errorf("ledger: BUG: device for key unavailable: %d", role)
	}

	// Query the public key from the device.
	rawPubKey, err := device.GetPublicKeyEd25519(signer.path)
	if err != nil {
		return pubKey, fmt.Errorf("ledger: failed to retrieive public key from device: %w", err)
	}
	if err = pubKey.UnmarshalBinary(rawPubKey); err != nil {
		return pubKey, fmt.Errorf("ledger: device returned malformed public key: %w", err)
	}
	signer.publicKey = &pubKey

	return pubKey, nil
}

func (pl *ledgerPlugin) ContextSign(
	role signature.SignerRole,
	rawContext signature.Context,
	message []byte,
) ([]byte, error) {
	signer, device, err := pl.signerForRole(role)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return nil, fmt.Errorf("ledger: BUG: device for key unavailable: %d", role)
	}

	preparedContext, err := signature.PrepareSignerContext(rawContext)
	if err != nil {
		return nil, fmt.Errorf("ledger: failed to prepare signing context: %w", err)
	}

	signature, err := device.SignEd25519(signer.path, preparedContext, message)
	if err != nil {
		return nil, fmt.Errorf("ledger: failed to sign message: %w", err)
	}

	return signature, nil
}

func (pl *ledgerPlugin) signerForRole(role signature.SignerRole) (*ledgerSigner, *internal.LedgerOasis, error) {
	signer := pl.inner[role]
	if signer == nil {
		// Plugin was not initialized with this role.
		return nil, nil, signature.ErrRoleMismatch
	}

	return signer, signer.device, nil
}

func main() {
	cmd.InitVersions(rootCmd)

	if err := rootCmd.Execute(); err != nil {
		// NOTE: The error is printed by cobra itself.
		os.Exit(1)
	}

	// Signer plugins use raw contexts.
	signature.UnsafeAllowUnregisteredContexts()

	var impl ledgerPlugin
	pluginSigner.Serve("ledger", &impl)
}
