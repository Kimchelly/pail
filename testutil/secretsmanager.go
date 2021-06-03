package testutil

import (
	"context"

	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/pkg/errors"
)

// MockSecretsManagerClient provides a mock implementation of a
// secretsmanager.Client, which can be used for testing purposes and
// introspection.
type MockSecretsManagerClient struct {
}

func (c *MockSecretsManagerClient) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput) (*secretsmanager.CreateSecretOutput, error) {
	return nil, errors.New("TODO: implement")
}

func (c *MockSecretsManagerClient) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput) (*secretsmanager.DescribeSecretOutput, error) {
	return nil, errors.New("TODO: implement")
}

func (c *MockSecretsManagerClient) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput) (*secretsmanager.DeleteSecretOutput, error) {
	return nil, errors.New("TODO: implement")
}

func (c *MockSecretsManagerClient) Close(ctx context.Context) error {
	return errors.New("TODO: implement")
}
