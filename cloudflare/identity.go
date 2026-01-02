package cloudflare

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/artbred/cloudflare-warp/cloudflare/crypto"
	"github.com/artbred/cloudflare-warp/cloudflare/model"
	"github.com/artbred/cloudflare-warp/core/datadir"
	"github.com/artbred/cloudflare-warp/log"
)

// CreateOrUpdateIdentity creates a new identity or updates an existing one.
// If identityDir is empty, uses the default data directory.
func CreateOrUpdateIdentity(identityDir string, license string) (*model.Identity, error) {
	if identityDir == "" {
		identityDir = datadir.GetDataDir()
	}

	// Ensure directory exists
	if err := os.MkdirAll(identityDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create identity directory: %w", err)
	}

	warpAPI := NewWarpAPI()

	identity, err := LoadIdentity(identityDir)
	if err != nil {
		log.Warnw("Failed to load existing WARP identity; attempting to create a new one", zap.Error(err))

		log.Info("Initiating creation of a new WARP identity...")
		newIdentity, err := CreateIdentity(warpAPI, license)
		if err != nil {
			return nil, err
		}

		// Save identity to the specified directory
		if err := saveIdentityToDir(&newIdentity, identityDir); err != nil {
			return nil, err
		}

		return &newIdentity, nil
	}

	if license != "" && identity.Account.License != license {
		log.Info("Attempting to update WARP account license key...")
		_, err := warpAPI.UpdateAccount(identity.Token, identity.ID, license)
		if err != nil {
			return nil, err
		}

		iAcc, err := warpAPI.GetAccount(identity.Token, identity.ID)
		if err != nil {
			return nil, err
		}
		identity.Account = iAcc

		// Save updated identity
		if err := saveIdentityToDir(identity, identityDir); err != nil {
			return nil, err
		}
	}

	return identity, nil
}

// LoadOrCreateIdentity loads an existing identity or creates a new one.
// If identityDir is empty, uses the default data directory.
func LoadOrCreateIdentity(identityDir string) (*model.Identity, error) {
	if identityDir == "" {
		identityDir = datadir.GetDataDir()
	}

	identity, err := LoadIdentity(identityDir)
	if err != nil {
		log.Warnw("Failed to load existing WARP identity; attempting to create a new one", zap.Error(err))
		log.Info("Initiating creation of a new WARP identity...")
		identity, err = CreateOrUpdateIdentity(identityDir, "")
		if err != nil {
			return nil, err
		}
	}

	log.Debug("Successfully loaded WARP identity.")
	return identity, nil
}

// LoadIdentity loads identity from specified directory.
// If identityDir is empty, uses the default data directory.
func LoadIdentity(identityDir string) (*model.Identity, error) {
	if identityDir == "" {
		identityDir = datadir.GetDataDir()
	}

	regPath := filepath.Join(identityDir, "reg.json")
	confPath := filepath.Join(identityDir, "conf.json")

	if _, err := os.Stat(regPath); os.IsNotExist(err) {
		return nil, err
	}
	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		return nil, err
	}

	regBytes, err := os.ReadFile(regPath)
	if err != nil {
		return nil, err
	}
	confBytes, err := os.ReadFile(confPath)
	if err != nil {
		return nil, err
	}

	var regFile model.RegFile
	if err := json.Unmarshal(regBytes, &regFile); err != nil {
		return nil, err
	}

	var confFile model.ConfFile
	if err := json.Unmarshal(confBytes, &confFile); err != nil {
		return nil, err
	}

	identity := model.Identity{
		ID:         regFile.RegistrationID,
		Token:      regFile.Token,
		PrivateKey: regFile.PrivateKey,
		Account:    confFile.Account,
		Config:     confFile.Config,
		Version:    "v2", // new version
	}

	if len(identity.Config.Peers) < 1 {
		return nil, errors.New("identity contains 0 peers")
	}

	return &identity, nil
}

func CreateIdentity(warpAPI *WarpAPI, license string) (model.Identity, error) {
	priv, err := crypto.GeneratePrivateKey()
	if err != nil {
		return model.Identity{}, err
	}

	privateKey, publicKey := priv.String(), priv.PublicKey().String()

	i, err := warpAPI.Register(publicKey)
	if err != nil {
		return model.Identity{}, err
	}

	if license != "" {
		log.Info("Attempting to update WARP account license key...")
		_, err := warpAPI.UpdateAccount(i.Token, i.ID, license)
		if err != nil {
			return model.Identity{}, err
		}

		ac, err := warpAPI.GetAccount(i.Token, i.ID)
		if err != nil {
			return model.Identity{}, err
		}
		i.Account = ac
	}

	i.PrivateKey = privateKey
	i.Version = "v2"

	return i, nil
}

// saveIdentityToDir saves an identity to the specified directory.
func saveIdentityToDir(identity *model.Identity, identityDir string) error {
	regPath := filepath.Join(identityDir, "reg.json")
	confPath := filepath.Join(identityDir, "conf.json")

	// Save reg.json
	regFileContent := model.RegFile{
		RegistrationID: identity.ID,
		Token:          identity.Token,
		PrivateKey:     identity.PrivateKey,
	}
	regData, err := json.MarshalIndent(regFileContent, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(regPath, regData, 0600); err != nil {
		return err
	}

	// Save conf.json
	confFileContent := model.ConfFile{
		Account: identity.Account,
		Config:  identity.Config,
	}
	confData, err := json.MarshalIndent(confFileContent, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(confPath, confData, 0600)
}
