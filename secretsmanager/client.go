package secretsmanager

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/secretsmanager"
)

// Client provides a common interface to interact with Secrets Manager and its
// mock implementation for testing. Implementations must handle retrying and
// backoff.
type Client interface {
	// CreateSecret creates a new secret.
	CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput) (*secretsmanager.CreateSecretOutput, error)
	// DescribeSecret provides information about an existing secret.
	DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput) (*secretsmanager.DescribeSecretOutput, error)
	// DeleteSecret deletes an existing secret.
	DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput) (*secretsmanager.DeleteSecretOutput, error)
	// Close closes the client and cleans up its resources. Implementations
	// should ensure that this is idempotent.
	Close(ctx context.Context) error
}

// SecretsManagerClient provides a Client implementation that wraps the Secrets Manager API.
type SecretsManagerClient struct {
}

func (c *SecretsManagerClient) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput) (*secretsmanager.CreateSecretOutput, error) {
	return nil, errors.New("TODO: implement")
}

func (c *SecretsManagerClient) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput) (*secretsmanager.DescribeSecretOutput, error) {
	return nil, errors.New("TODO: implement")
}

func (c *SecretsManagerClient) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput) (*secretsmanager.DeleteSecretOutput, error) {
	return nil, errors.New("TODO: implement")
}

func (c *SecretsManagerClient) Close(ctx context.Context) error {
	return errors.New("TODO: implement")
}
