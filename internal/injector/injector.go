// Copyright © 2020 Banzai Cloud
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

package injector

import (
	"encoding/json"
	"regexp"
	"strings"

	"emperror.dev/errors"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cast"

	"github.com/banzaicloud/bank-vaults/internal/configuration"
	"github.com/banzaicloud/bank-vaults/pkg/sdk/vault"
)

type SecretInjectorFunc func(key, value string)

type SecretRenewer interface {
	Renew(path string, secret *vaultapi.Secret) error
}

type Config struct {
	TransitKeyID         string
	TransitPath          string
	TransitBatchSize     int
	IgnoreMissingSecrets bool
	DaemonMode           bool
}

type SecretInjector struct {
	config       Config
	client       *vault.Client
	renewer      SecretRenewer
	logger       logrus.FieldLogger
	transitCache map[string][]byte
	secretCache  map[string]map[string]interface{}
}

func NewSecretInjector(config Config, client *vault.Client, renewer SecretRenewer, logger logrus.FieldLogger) SecretInjector {
	return SecretInjector{
		config:       config,
		client:       client,
		renewer:      renewer,
		logger:       logger,
		transitCache: map[string][]byte{},
		secretCache:  map[string]map[string]interface{}{},
	}
}

var inlineMutationRegex = regexp.MustCompile(`\${([>]{0,2}vault:.*?)}`)

func (i SecretInjector) FetchTransitSecrets(secrets []string) (map[string][]byte, error) {
	if len(i.config.TransitKeyID) == 0 {
		return map[string][]byte{}, errors.Errorf("found encrypted variable, but transit key ID is empty: %s", "todo")
	}

	if len(secrets) == 0 {
		return map[string][]byte{}, nil
	}

	out, err := i.client.Transit.DecryptBatch(i.config.TransitPath, i.config.TransitKeyID, secrets)
	for k, v := range out {
		i.transitCache[k] = v
	}

	if err != nil {
		i.logger.Errorln("failed to decrypt variable:", err)
	}

	return out, nil
}

func paginate(secrets []string, batchSize int) [][]string {
	transitSecrets := [][]string{}

	for i := range secrets {
		if i%batchSize == 0 {
			transitSecrets = append(transitSecrets, []string{})
		}

		index := i / batchSize

		transitSecrets[index] = append(transitSecrets[index], secrets[i])
	}

	return transitSecrets
}

func (i SecretInjector) preprocessTransitSecrets(references *map[string]string, inject SecretInjectorFunc) error {
	// use set so that we don't have duplicates
	secretSet := map[string]bool{}

	for _, value := range *references {
		// decrypts value with Vault Transit Secret Engine
		if HasInlineVaultDelimiters(value) {
			for _, vaultSecretReference := range FindInlineVaultDelimiters(value) {
				if i.client.Transit.IsEncrypted(vaultSecretReference[1]) {
					secretSet[vaultSecretReference[1]] = true
				}
			}
		} else if i.client.Transit.IsEncrypted(value) {
			secretSet[value] = true
		}
	}

	// convert back to slice & filter out already-cached secrets
	secrets := make([]string, 0, len(secretSet))
	for k := range secretSet {
		if _, cached := i.transitCache[k]; !cached {
			secrets = append(secrets, k)
		}
	}

	for _, sec := range paginate(secrets, i.config.TransitBatchSize) {
		_, err := i.FetchTransitSecrets(sec)
		if err != nil {
			if !i.config.IgnoreMissingSecrets {
				return errors.Wrapf(err, "failed to decrypt secret: %s", sec)
			}

			i.logger.Errorln("failed to decrypt secret:", sec, err)
		}
	}

	for name, value := range *references {
		if HasInlineVaultDelimiters(value) {
			for _, vaultSecretReference := range FindInlineVaultDelimiters(value) {
				if v, ok := i.transitCache[vaultSecretReference[0]]; ok {
					value = strings.Replace(value, vaultSecretReference[0], string(v), -1)
				}
			}
			inject(name, value)

			continue
		}
		if i.client.Transit.IsEncrypted(value) {
			if v, ok := i.transitCache[value]; ok {
				inject(name, string(v))

				continue
			}
		}
	}

	return nil
}

func (i SecretInjector) InjectSecretsFromVault(references map[string]string, inject SecretInjectorFunc) error {
	err := i.preprocessTransitSecrets(&references, inject)
	if err != nil && !i.config.IgnoreMissingSecrets {
		return errors.Wrapf(err, "unable to preprocess transit secrets")
	}

	for name, value := range references {
		if HasInlineVaultDelimiters(value) {
			for _, vaultSecretReference := range FindInlineVaultDelimiters(value) {
				mapData, err := i.GetDataFromVault(map[string]string{name: vaultSecretReference[1]})
				if err != nil {
					return err
				}
				for _, v := range mapData {
					value = strings.Replace(value, vaultSecretReference[0], v, -1)
				}
			}
			inject(name, value)

			continue
		}

		var update bool
		if strings.HasPrefix(value, ">>vault:") {
			value = strings.TrimPrefix(value, ">>")
			update = true
		} else {
			update = false
		}

		if !strings.HasPrefix(value, "vault:") {
			inject(name, value)

			continue
		}

		valuePath := strings.TrimPrefix(value, "vault:")

		// handle special case for vault:login env value
		// namely pass through the VAULT_TOKEN received from the Vault login procedure
		if name == "VAULT_TOKEN" && valuePath == "login" {
			value = i.client.RawClient().Token()
			inject(name, value)

			continue
		}

		// decrypts value with Vault Transit Secret Engine
		if i.client.Transit.IsEncrypted(value) {
			if len(i.config.TransitKeyID) == 0 {
				return errors.Errorf("found encrypted variable, but transit key ID is empty: %s", name)
			}

			if v, ok := i.transitCache[value]; ok {
				inject(name, string(v))

				continue
			}

			out, err := i.client.Transit.Decrypt(i.config.TransitPath, i.config.TransitKeyID, []byte(value))
			if err != nil {
				if !i.config.IgnoreMissingSecrets {
					return errors.Wrapf(err, "failed to decrypt variable: %s", name)
				}
				i.logger.Errorln("failed to decrypt variable:", name, err)

				continue
			}

			i.transitCache[value] = out
			inject(name, string(out))

			continue
		}

		split := strings.SplitN(valuePath, "#", 3)
		valuePath = split[0]

		if len(split) < 2 {
			return errors.New("secret data key or template not defined")
		}

		key := split[1]

		versionOrData := "-1"
		if update {
			versionOrData = "{}"
		}
		if len(split) == 3 {
			versionOrData = split[2]
		}

		secretCacheKey := valuePath + "#" + versionOrData
		var data map[string]interface{}
		var err error

		if data = i.secretCache[secretCacheKey]; data == nil {
			data, err = i.readVaultPath(valuePath, versionOrData, update)
		}

		if err != nil {
			return err
		}

		if data == nil {
			if !i.config.IgnoreMissingSecrets {
				return errors.Errorf("path not found: %s", valuePath)
			}

			i.logger.Errorf("path not found: %s", valuePath)

			continue
		}

		i.secretCache[secretCacheKey] = data

		templater := configuration.NewTemplater(configuration.DefaultLeftDelimiter, configuration.DefaultRightDelimiter)

		if templater.IsGoTemplate(key) {
			value, err := templater.Template(key, data)
			if err != nil {
				return errors.Wrapf(err, "failed to interpolate template key with vault data: %s", key)
			}
			inject(name, value.String())
		} else {
			if value, ok := data[key]; ok {
				value, err := cast.ToStringE(value)
				if err != nil {
					return errors.Wrap(err, "value can't be cast to a string")
				}
				inject(name, value)
			} else {
				return errors.Errorf("key '%s' not found under path: %s", key, valuePath)
			}
		}
	}

	return nil
}

func (i SecretInjector) InjectSecretsFromVaultPath(paths string, inject SecretInjectorFunc) error {
	vaultPaths := strings.Split(paths, ",")

	for _, path := range vaultPaths {
		split := strings.SplitN(path, "#", 2)
		valuePath := split[0]

		version := "-1"

		if len(split) > 2 {
			version = split[2]
		}

		data, err := i.readVaultPath(valuePath, version, false)
		if err != nil {
			return err
		}

		if data == nil {
			if !i.config.IgnoreMissingSecrets {
				return errors.Errorf("path not found: %s", valuePath)
			}

			i.logger.Errorln("path not found:", valuePath)

			continue
		}

		for key, value := range data {
			value, err := cast.ToStringE(value)
			if err != nil {
				return errors.Wrap(err, "value can't be cast to a string for key: "+key)
			}
			inject(key, value)
		}
	}

	return nil
}

func (i SecretInjector) readVaultPath(path, versionOrData string, update bool) (map[string]interface{}, error) {
	var secretData map[string]interface{}

	var secret *vaultapi.Secret
	var err error

	if update {
		var data map[string]interface{}
		err = json.Unmarshal([]byte(versionOrData), &data)
		if err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal data for writing")
		}

		secret, err = i.client.RawClient().Logical().Write(path, data)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to write secret to path: %s", path)
		}
	} else {
		secret, err = i.client.RawClient().Logical().ReadWithData(path, map[string][]string{"version": {versionOrData}})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read secret from path: %s", path)
		}
	}

	if i.config.DaemonMode && secret != nil && secret.LeaseDuration > 0 {
		i.logger.Infof("secret %s has a lease duration of %ds, starting renewal", path, secret.LeaseDuration)

		err = i.renewer.Renew(path, secret)
		if err != nil {
			return nil, errors.Wrap(err, "secret renewal can't be established")
		}
	}

	if secret == nil {
		return nil, nil
	}

	for _, warning := range secret.Warnings {
		i.logger.Warnf("%s: %s", path, warning)
	}

	v2Data, ok := secret.Data["data"]
	if ok {
		secretData = cast.ToStringMap(v2Data)

		// Check if a given version of a path is destroyed
		metadata := secret.Data["metadata"].(map[string]interface{}) // nolint:forcetypeassert
		if metadata["destroyed"].(bool) {
			i.logger.Warnln("version of secret has been permanently destroyed version:", versionOrData, "path:", path)
		}

		// Check if a given version of a path still exists
		if deletionTime, ok := metadata["deletion_time"].(string); ok && deletionTime != "" {
			i.logger.Warnln("cannot find data for path, given version has been deleted",
				"path:", path, "version:", versionOrData,
				"deletion_time", deletionTime)
		}
	} else {
		secretData = cast.ToStringMap(secret.Data)
	}

	return secretData, nil
}

func HasInlineVaultDelimiters(value string) bool {
	return len(FindInlineVaultDelimiters(value)) > 0
}

func FindInlineVaultDelimiters(value string) [][]string {
	return inlineMutationRegex.FindAllStringSubmatch(value, -1)
}

func (i SecretInjector) GetDataFromVault(data map[string]string) (map[string]string, error) {
	vaultData := make(map[string]string, len(data))

	inject := func(key, value string) {
		vaultData[key] = value
	}

	return vaultData, i.InjectSecretsFromVault(data, inject)
}
