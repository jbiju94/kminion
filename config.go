package main

import (
	"encoding/json"
	"fmt"
	"github.com/cloudhut/kminion/v2/kafka"
	"github.com/cloudhut/kminion/v2/logging"
	"github.com/cloudhut/kminion/v2/minion"
	"github.com/cloudhut/kminion/v2/prometheus"
	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/mitchellh/mapstructure"
	"go.uber.org/zap"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
)

type Config struct {
	Version string `koanf:"version"`

	Kafka    kafka.Config      `koanf:"kafka"`
	Minion   minion.Config     `koanf:"minion"`
	Exporter prometheus.Config `koanf:"exporter"`
	Logger   logging.Config    `koanf:"logger"`
}

func (c *Config) SetDefaults() {
	c.Kafka.SetDefaults()
	c.Minion.SetDefaults()
	c.Exporter.SetDefaults()
	c.Logger.SetDefaults()
}

func (c *Config) Validate() error {
	err := c.Kafka.Validate()
	if err != nil {
		return fmt.Errorf("failed to validate kafka config: %w", err)
	}

	err = c.Minion.Validate()
	if err != nil {
		return fmt.Errorf("failed to validate minion config: %w", err)
	}

	err = c.Logger.Validate()
	if err != nil {
		return fmt.Errorf("failed to validate logger config: %w", err)
	}

	return nil
}

func newConfig(logger *zap.Logger) (Config, error) {
	k := koanf.New(".")
	var cfg Config
	cfg.SetDefaults()

	// 1. Check if a config filepath is set via flags. If there is one we'll try to load the file using a YAML Parser
	envKey := "CONFIG_FILEPATH"
	configFilepath := os.Getenv(envKey)
	if configFilepath == "" {
		logger.Info("the env variable '" + envKey + "' is not set, therefore no YAML config will be loaded")
	} else {
		err := k.Load(file.Provider(configFilepath), yaml.Parser())
		if err != nil {
			return Config{}, fmt.Errorf("failed to parse YAML config: %w", err)
		}
	}

	// We could unmarshal the loaded koanf input after loading both providers, however we want to unmarshal the YAML
	// config with `ErrorUnused` set to true, but unmarshal environment variables with `ErrorUnused` set to false (default).
	// Rationale: Orchestrators like Kubernetes inject unrelated environment variables, which we still want to allow.
	err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag:       "",
		FlatPaths: false,
		DecoderConfig: &mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc()),
			Metadata:         nil,
			Result:           &cfg,
			WeaklyTypedInput: true,
			ErrorUnused:      true,
		},
	})
	if err != nil {
		return Config{}, err
	}

	err = k.Load(env.ProviderWithValue("", ".", func(s string, v string) (string, interface{}) {
		// key := strings.Replace(strings.ToLower(s), "_", ".", -1)
		key := strings.Replace(strings.ToLower(s), "_", ".", -1)
		// Check to exist if we have a configuration option already and see if it's a slice
		// If there is a comma in the value, split the value into a slice by the comma.
		if strings.Contains(v, ",") {
			return key, strings.Split(v, ",")
		}

		// Otherwise return the new key with the unaltered value
		return key, v
	}), nil)
	if err != nil {
		return Config{}, err
	}

	err = k.Unmarshal("", &cfg)
	if err != nil {
		return Config{}, err
	}

	err = cfg.Validate()
	if err != nil {
		return Config{}, fmt.Errorf("failed to validate config: %w", err)
	}

	// VCAP Specifications
	type Cluster struct {
		Brokers string
	}

	type Urls struct {
		CaCert      string `json:"ca_cert"`
		Certs       string `json:"certs"`
		CertCurrent string `json:"cert_current"`
		CertNext    string `json:"cert_next"`
		Token       string `json:"token"`
	}

	type Credentials struct {
		Username string
		Password string
		Cluster  Cluster
		Urls     Urls
	}

	type Kafka struct {
		Credentials Credentials
		Name        string
	}

	type VCAP struct {
		Kafka []Kafka
	}

	type Token struct {
		AccessToken string `json:"access_token"`
	}

	vcap, vcapPresent := os.LookupEnv("VCAP_SERVICES")
	if vcapPresent {
		var vcapStruct VCAP
		err := json.Unmarshal([]byte(vcap), &vcapStruct)
		if err != nil {
			return Config{}, fmt.Errorf("Env read Failed: %w", err)
		}
		caURL := vcapStruct.Kafka[0].Credentials.Urls.CertCurrent
		tokenURL := vcapStruct.Kafka[0].Credentials.Urls.Token
		err1 := DownloadCertificate(caURL, "current.cer")
		if err1 != nil {
			return Config{}, fmt.Errorf("CA Certificate download failed: %w", err)
		}

		cfg.Kafka.Brokers = strings.Split(vcapStruct.Kafka[0].Credentials.Cluster.Brokers, ",")
		cfg.Kafka.SASL.Enabled = true
		cfg.Kafka.SASL.Mechanism = "PLAIN"

		basicAuthUserName := vcapStruct.Kafka[0].Credentials.Username
		basicAuthPassword := vcapStruct.Kafka[0].Credentials.Password
		cfg.Kafka.SASL.Username = basicAuthUserName
		tokenString, err := getToken(tokenURL, basicAuthUserName, basicAuthPassword)
		if err != nil {
			logger.Error("Kafka Auth Error: Token Fetch Failed")
		}

		token := Token{}
		err2 := json.Unmarshal([]byte(tokenString), &token)
		if err2 != nil {
			return Config{}, fmt.Errorf("Token Fetch Failed: %w", err)
		}
		cfg.Kafka.SASL.Password = token.AccessToken

		cfg.Kafka.TLS.Enabled = true
		cfg.Kafka.TLS.InsecureSkipTLSVerify = true
		cfg.Kafka.TLS.CaFilepath = "./current.cer"

		e, err := json.Marshal(cfg.Kafka)
		logger.Info("Kafka Config:" + string(e))

	}

	return cfg, nil
}

func getToken(url string, username string, password string) (string, error) {

	method := "POST"
	payload := strings.NewReader("grant_type=client_credentials")

	client := &http.Client{}
	req, err := http.NewRequest(method, url, payload)

	if err != nil {
		return "", err
	}
	req.SetBasicAuth(username, password)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	return string(body), err
}

func DownloadCertificate(url string, filename string) error {

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	return err
}
