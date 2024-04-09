// Copyright 2019 - MinIO, Inc. All rights reserved.
// Use of this source code is governed by the AGPLv3
// license that can be found in the LICENSE file.

package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/minio/kes"
	"github.com/minio/kes/internal/keystore"
	kesdk "github.com/minio/kms-go/kes"
)

// Credentials represents static AWS credentials:
// access key, secret key and a session token
type Credentials struct {
	AccessKey    string // The AWS access key
	SecretKey    string // The AWS secret key
	SessionToken string // The AWS session token
}

// Config is a structure containing configuration
// options for connecting to the AWS SecretsManager.
type Config struct {
	// Addr is the HTTP address of the AWS Secret
	// Manager. In general, the address has the
	// following form:
	//  secretsmanager.<region>.amazonaws.com
	Addr string

	// Region is the AWS region. Even though the Addr
	// endpoint contains that information already, this
	// field is mandatory.
	Region string

	// The KMSKeyID is the AWS-KMS key ID specifying the
	// AWS-KMS key that is used to encrypt (and decrypt) the
	// values stored at AWS Secrets Manager.
	KMSKeyID string

	// Login contains the AWS credentials (access/secret key).
	Login Credentials
}

// Connect establishes and returns a Conn to a AWS SecretManager
// using the given config.
func Connect(ctx context.Context, config *Config) (*Store, error) {
	credentials := credentials.NewStaticCredentials(
		config.Login.AccessKey,
		config.Login.SecretKey,
		config.Login.SessionToken,
	)
	if config.Login.AccessKey == "" && config.Login.SecretKey == "" && config.Login.SessionToken == "" {
		// If all login credentials (access key, secret key and session token) are empty
		// we pass no (not empty) credentials to the AWS SDK. The SDK will try to fetch
		// the credentials from:
		//  - Environment Variables
		//  - Shared Credentials file
		//  - EC2 Instance Metadata
		// In particular, when running a kes server on an EC2 instance, the SDK will
		// automatically fetch the temp. credentials from the EC2 metadata service.
		// See: AWS IAM roles for EC2 instances.
		credentials = nil
	}

	session, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Endpoint:    aws.String(config.Addr),
			Region:      aws.String(config.Region),
			Credentials: credentials,
		},
		SharedConfigState: session.SharedConfigDisable,
	})
	if err != nil {
		return nil, err
	}

	c := &Store{
		config: *config,
		client: secretsmanager.New(session),
	}
	if _, err = c.Status(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Store is an AWS SecretsManager secret store.
type Store struct {
	config Config
	client *secretsmanager.SecretsManager
}

func (s *Store) String() string { return "AWS SecretsManager: " + s.config.Addr }

// Status returns the current state of the AWS SecretsManager instance.
// In particular, whether it is reachable and the network latency.
func (s *Store) Status(ctx context.Context) (kes.KeyStoreState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.client.Endpoint, nil)
	if err != nil {
		return kes.KeyStoreState{}, err
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return kes.KeyStoreState{}, &keystore.ErrUnreachable{Err: err}
	}
	defer resp.Body.Close()

	return kes.KeyStoreState{
		Latency: time.Since(start),
	}, nil
}

// Create stores the given key-value pair at the AWS SecretsManager
// if and only if it doesn't exists. If such an entry already exists
// it returns kes.ErrKeyExists.
//
// If the SecretsManager.KMSKeyID is set AWS will use this key ID to
// encrypt the values. Otherwise, AWS will use the default key ID for
// encrypting secrets at the AWS SecretsManager.
func (s *Store) Create(ctx context.Context, name string, value string) error {
	createOpt := secretsmanager.CreateSecretInput{
		Name:         aws.String(name),
		SecretString: aws.String(value),
	}
	if s.config.KMSKeyID != "" {
		createOpt.KmsKeyId = aws.String(s.config.KMSKeyID)
	}
	if _, err := s.client.CreateSecretWithContext(ctx, &createOpt); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err, ok := err.(awserr.Error); ok {
			if err.Code() == secretsmanager.ErrCodeResourceExistsException {
				return kesdk.ErrKeyExists
			}
		}
		return fmt.Errorf("aws: failed to create '%s': %v", name, err)
	}
	return nil
}

// Get returns the value associated with the given key.
// If no entry for key exists, it returns kes.ErrKeyNotFound.
func (s *Store) Get(ctx context.Context, name string) (string, error) {
	response, err := s.client.GetSecretValueWithContext(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(name),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if err, ok := err.(awserr.Error); ok {
			switch err.Code() {
			case secretsmanager.ErrCodeDecryptionFailure:
				return "", fmt.Errorf("aws: cannot access '%s': %v", name, err)
			case secretsmanager.ErrCodeResourceNotFoundException:
				return "", kesdk.ErrKeyNotFound
			}
		}
		return "", fmt.Errorf("aws: failed to read '%s': %v", name, err)
	}

	if response.SecretString == nil {
		return "", nil
	}
	return *response.SecretString, nil
}

// Delete removes the key-value pair from the AWS SecretsManager, if
// it exists.
func (s *Store) Delete(ctx context.Context, name string) error {
	_, err := s.client.DeleteSecretWithContext(ctx, &secretsmanager.DeleteSecretInput{
		SecretId:                   aws.String(name),
		ForceDeleteWithoutRecovery: aws.Bool(true),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err, ok := err.(awserr.Error); ok {
			if err.Code() == secretsmanager.ErrCodeResourceNotFoundException {
				return kesdk.ErrKeyNotFound
			}
		}
		return fmt.Errorf("aws: failed to delete '%s': %v", name, err)
	}
	return nil
}

// List returns a new Iterator over the names of
// all stored keys.
// List returns the first n key names, that start with the given
// prefix, and the next prefix from which the listing should
// continue.
//
// It returns all keys with the prefix if n < 0 and less than n
// names if n is greater than the number of keys with the prefix.
//
// An empty prefix matches any key name. At the end of the listing
// or when there are no (more) keys starting with the prefix, the
// returned prefix is empty.
func (s *Store) List(ctx context.Context, prefix string, n int) ([]string, string, error) {
	var names []string
	err := s.client.ListSecretsPagesWithContext(ctx, &secretsmanager.ListSecretsInput{}, func(page *secretsmanager.ListSecretsOutput, lastPage bool) bool {
		for _, secret := range page.SecretList {
			names = append(names, *secret.Name)
		}

		// The pagination is stopped once we return false.
		// If lastPage is true then we reached the end. Therefore,
		// we return !lastPage which then is false.
		return !lastPage
	})
	if err != nil {
		return nil, "", err
	}
	return keystore.List(names, prefix, n)
}

// Close closes the Store.
func (s *Store) Close() error { return nil }
