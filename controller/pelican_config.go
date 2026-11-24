package controller

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const pelicanConfigKey = "PelicanGroupMonitorSecret"

var pelicanConfigMu sync.Mutex

type pelicanGroupTarget struct {
	ID              string   `json:"id"`
	Group           string   `json:"group"`
	BaseURL         string   `json:"base_url"`
	APIKey          string   `json:"api_key,omitempty"`
	HasAPIKey       bool     `json:"has_api_key"`
	Models          []string `json:"models"`
	Endpoint        string   `json:"endpoint"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Enabled         bool     `json:"enabled"`
}
type pelicanGroupConfig struct {
	Enabled bool                 `json:"enabled"`
	Targets []pelicanGroupTarget `json:"targets"`
}

func (config pelicanGroupConfig) targetByID(id string) (pelicanGroupTarget, bool) {
	for _, target := range config.Targets {
		if target.ID == id {
			return target, true
		}
	}
	return pelicanGroupTarget{}, false
}

// The option name is secret-filtered by the generic settings API. Credentials
// are authenticated-encrypted at rest and never returned by the config API.
func readPelicanConfig(db *gorm.DB) (pelicanGroupConfig, error) {
	config := pelicanGroupConfig{Targets: []pelicanGroupTarget{}}
	var option model.Option
	err := db.Where(&model.Option{Key: pelicanConfigKey}).First(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	err = common.UnmarshalJsonStr(option.Value, &config)
	if config.Targets == nil {
		config.Targets = []pelicanGroupTarget{}
	}
	return config, err
}

func pelicanCredentialCipher() (cipher.AEAD, error) {
	if os.Getenv("CRYPTO_SECRET") == "" && os.Getenv("SESSION_SECRET") == "" {
		return nil, errors.New("Configure a persistent CRYPTO_SECRET before saving monitor credentials")
	}
	key := sha256.Sum256([]byte("pelican-group-monitor-v2\x00" + common.CryptoSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func pelicanKey(target pelicanGroupTarget) (string, error) {
	aead, err := pelicanCredentialCipher()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(target.APIKey)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, nil, data, []byte(target.ID+"\x00"+target.BaseURL+"\x00"+target.Group))
	return string(plain), err
}

func localPelicanTarget(target pelicanGroupTarget) bool {
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return false
	}
	local, err := url.Parse(system_setting.ServerAddress)
	if err != nil {
		return false
	}
	return strings.EqualFold(base.Scheme, local.Scheme) && strings.EqualFold(base.Host, local.Host) && strings.TrimSuffix(base.Path, "/v1") == strings.TrimRight(local.Path, "/")
}

// A gateway token determines routing. Never override routing with a label,
// channel ID, or a made-up group parameter in the inference request.
func validatePelicanTokenGroup(target pelicanGroupTarget, key string) error {
	if !localPelicanTarget(target) {
		return nil
	} // Remote binding is operator supplied.
	token, err := model.GetTokenByKey(strings.TrimPrefix(key, "sk-"), false)
	if err != nil || token == nil {
		return errors.New("API key is not a local gateway token")
	}
	group := token.Group
	if group == "" {
		user, err := model.GetUserById(token.UserId, false)
		if err != nil {
			return errors.New("Token owner unavailable")
		}
		group = user.Group
	}
	if group == "auto" || token.CrossGroupRetry || group != target.Group {
		return errors.New("API key must be bound to this fixed group without cross-group retry")
	}
	return nil
}

func normalizePelicanTarget(target *pelicanGroupTarget, previous map[string]pelicanGroupTarget) error {
	target.Group = strings.TrimSpace(target.Group)
	target.BaseURL = strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	u, err := url.Parse(target.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(target.BaseURL) > 2048 {
		return errors.New("Site URL must be an HTTPS URL without credentials, query or fragment")
	}
	if target.Group == "" || target.Group == "auto" || len(target.Group) > 128 {
		return errors.New("A fixed group name is required")
	}
	if target.Endpoint != "chat/completions" && target.Endpoint != "responses" {
		return errors.New("Choose chat/completions or responses")
	}
	if target.ReasoningEffort != "" && target.ReasoningEffort != "medium" {
		return errors.New("Choose the model default or medium reasoning effort")
	}
	if len(target.Models) == 0 || len(target.Models) > 10 {
		return errors.New("Configure between 1 and 10 explicit models per group")
	}
	models := []string{}
	for _, name := range target.Models {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 255 {
			return errors.New("Invalid model name")
		}
		if !slices.Contains(models, name) {
			models = append(models, name)
		}
	}
	target.Models = models
	old, exists := previous[target.ID]
	if target.ID == "" {
		target.ID = common.GetRandomString(24)
	} else if !exists {
		return errors.New("Unknown monitoring target")
	}
	key := strings.TrimSpace(target.APIKey)
	if key == "" {
		if !exists || old.BaseURL != target.BaseURL || old.Group != target.Group {
			return errors.New("Enter the group API key; changing site or group requires a new key")
		}
		target.APIKey = old.APIKey
		key, err = pelicanKey(*target)
		if err != nil {
			return errors.New("Stored key cannot be decrypted; enter it again")
		}
	} else {
		if len(key) > 4096 || strings.ContainsAny(key, "\r\n \t") {
			return errors.New("Invalid API key")
		}
		aead, err := pelicanCredentialCipher()
		if err != nil {
			return err
		}
		target.APIKey = base64.StdEncoding.EncodeToString(aead.Seal(nil, nil, []byte(key), []byte(target.ID+"\x00"+target.BaseURL+"\x00"+target.Group)))
	}
	target.HasAPIKey = true
	return validatePelicanTokenGroup(*target, key)
}

func GetPelicanConfig(c *gin.Context) {
	config, err := readPelicanConfig(model.DB.WithContext(c.Request.Context()))
	if err != nil {
		c.JSON(503, gin.H{"success": false, "message": "Monitor configuration unavailable"})
		return
	}
	for i := range config.Targets {
		config.Targets[i].HasAPIKey = config.Targets[i].APIKey != ""
		config.Targets[i].APIKey = ""
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"success": true, "data": config})
}

func UpdatePelicanConfig(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	var config pelicanGroupConfig
	if err := common.DecodeJson(c.Request.Body, &config); err != nil || len(config.Targets) > 50 {
		c.JSON(400, gin.H{"success": false, "message": "Invalid monitor configuration"})
		return
	}
	if config.Targets == nil {
		config.Targets = []pelicanGroupTarget{}
	}
	pelicanConfigMu.Lock()
	defer pelicanConfigMu.Unlock()
	err := model.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		old, err := readPelicanConfig(tx)
		if err != nil {
			return errors.New("Monitor configuration unavailable")
		}
		previous := map[string]pelicanGroupTarget{}
		for _, target := range old.Targets {
			previous[target.ID] = target
		}
		groups := map[string]bool{}
		ids := map[string]bool{}
		for i := range config.Targets {
			target := &config.Targets[i]
			if err := normalizePelicanTarget(target, previous); err != nil {
				return err
			}
			if groups[target.Group] || ids[target.ID] {
				return errors.New("Each group may appear only once")
			}
			groups[target.Group] = true
			ids[target.ID] = true
		}
		body, err := common.Marshal(config)
		if err != nil {
			return err
		}
		option := model.Option{Key: pelicanConfigKey, Value: string(body)}
		// Save uses the existing portable options primary key; no new secret table.
		return tx.Save(&option).Error
	})
	if err != nil {
		c.JSON(400, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"success": true})
}
