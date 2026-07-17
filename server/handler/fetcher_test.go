// Copyright 2026 Palantir Technologies, Inc.
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

package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-github/v85/github"
	"github.com/palantir/go-githubapp/appconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockConfigLoader struct {
	loadConfig func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error)
}

func (s mockConfigLoader) LoadConfig(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
	return s.loadConfig(ctx, client, owner, repo, ref)
}

func TestConfigFetcherMarksSeenPolicy(t *testing.T) {
	cache := NewSeenPolicyCache()

	fetcher := ConfigFetcher{
		Loader: mockConfigLoader{
			loadConfig: func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
				return appconfig.Config{
					Content: []byte("policy: ["),
					Source:  "testorg/testrepo@main",
					Path:    ".policy.yml",
				}, nil
			},
		},
		SeenPolicyCache: cache,
	}

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), nil, "testorg", "testrepo", "main")
	require.Error(t, fc.ParseError)
	assert.True(t, fc.SeenPolicy)

	ok := cache.Get(SeenPolicyKey{
		Owner:      "testorg",
		Repository: "testrepo",
		BaseBranch: "main",
	})
	require.True(t, ok)
}

func TestConfigFetcherChecksSeenPolicyOnLoadError(t *testing.T) {
	cache := NewSeenPolicyCache()
	cache.Set(SeenPolicyKey{
		Owner:      "testorg",
		Repository: "testrepo",
		BaseBranch: "main",
	})

	fetcher := ConfigFetcher{
		Loader: mockConfigLoader{
			loadConfig: func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
				return appconfig.Config{
					Source: "testorg/testrepo@main",
					Path:   ".policy.yml",
				}, errors.New("request failed")
			},
		},
		SeenPolicyCache: cache,
	}

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), nil, "testorg", "testrepo", "main")
	require.Error(t, fc.LoadError)
	assert.True(t, fc.SeenPolicy)
}

func TestConfigFetcherScopesSeenPolicyByBranch(t *testing.T) {
	const releaseBranch = "release"

	cache := NewSeenPolicyCache()
	cache.Set(SeenPolicyKey{
		Owner:      "testorg",
		Repository: "testrepo",
		BaseBranch: "main",
	})

	fetcher := ConfigFetcher{
		Loader: mockConfigLoader{
			loadConfig: func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
				return appconfig.Config{
					Source: "testorg/testrepo@" + releaseBranch,
					Path:   ".policy.yml",
				}, errors.New("request failed")
			},
		},
		SeenPolicyCache: cache,
	}

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), nil, "testorg", "testrepo", releaseBranch)
	require.Error(t, fc.LoadError)
	assert.False(t, fc.SeenPolicy)
}

func githubStatusError(code int) error {
	return &github.ErrorResponse{
		Response: &http.Response{StatusCode: code},
	}
}

func githubBodyError(code int, message string, errs ...github.Error) error {
	return &github.ErrorResponse{
		Response: &http.Response{StatusCode: code},
		Message:  message,
		Errors:   errs,
	}
}

func TestIsRetryableError(t *testing.T) {
	for _, test := range []struct {
		code      int
		retryable bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusUnprocessableEntity, false},
	} {
		assert.Equal(t, test.retryable, isRetryableError(githubStatusError(test.code)), "status %d", test.code)
	}

	assert.False(t, isRetryableError(errors.New("request failed")))

	// GitHub intermittently answers a valid request with a bodyless 400; a real
	// 400 always says what was wrong with the request.
	assert.True(t, isRetryableError(githubStatusError(http.StatusBadRequest)))
	assert.False(t, isRetryableError(githubBodyError(http.StatusBadRequest, "Invalid request")))
	assert.False(t, isRetryableError(githubBodyError(http.StatusBadRequest, "", github.Error{Field: "ref", Code: "invalid"})))
}

func TestConfigFetcherRetriesBadGateway(t *testing.T) {
	calls := 0

	fetcher := ConfigFetcher{
		Loader: mockConfigLoader{
			loadConfig: func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
				calls++
				if calls == 1 {
					return appconfig.Config{}, githubStatusError(http.StatusBadGateway)
				}
				return appconfig.Config{
					Content: []byte("policy:\n  approval:\n    - rule\n"),
					Source:  "testorg/testrepo@main",
					Path:    ".policy.yml",
				}, nil
			},
		},
		SeenPolicyCache: NewSeenPolicyCache(),
	}

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), nil, "testorg", "testrepo", "main")
	require.NoError(t, fc.LoadError)
	require.NoError(t, fc.ParseError)
	assert.Equal(t, 2, calls)
	assert.True(t, fc.SeenPolicy)
}

func TestConfigFetcherRetriesEmptyBadRequest(t *testing.T) {
	calls := 0

	fetcher := ConfigFetcher{
		Loader: mockConfigLoader{
			loadConfig: func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
				calls++
				if calls == 1 {
					return appconfig.Config{}, githubStatusError(http.StatusBadRequest)
				}
				return appconfig.Config{
					Content: []byte("policy:\n  approval:\n    - rule\n"),
					Source:  "testorg/testrepo@main",
					Path:    ".policy.yml",
				}, nil
			},
		},
		SeenPolicyCache: NewSeenPolicyCache(),
	}

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), nil, "testorg", "testrepo", "main")
	require.NoError(t, fc.LoadError)
	require.NoError(t, fc.ParseError)
	assert.Equal(t, 2, calls)
	assert.True(t, fc.SeenPolicy)
}

func TestConfigFetcherGivesUpOnPersistentEmptyBadRequest(t *testing.T) {
	calls := 0

	fetcher := ConfigFetcher{
		Loader: mockConfigLoader{
			loadConfig: func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
				calls++
				return appconfig.Config{}, githubStatusError(http.StatusBadRequest)
			},
		},
		SeenPolicyCache: NewSeenPolicyCache(),
	}

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), nil, "testorg", "testrepo", "main")
	require.Error(t, fc.LoadError)
	assert.Equal(t, 4, calls)
}

func TestConfigFetcherDoesNotRetryMeaningfulBadRequest(t *testing.T) {
	calls := 0

	fetcher := ConfigFetcher{
		Loader: mockConfigLoader{
			loadConfig: func(ctx context.Context, client *github.Client, owner, repo, ref string) (appconfig.Config, error) {
				calls++
				return appconfig.Config{}, githubBodyError(http.StatusBadRequest, "Invalid request")
			},
		},
		SeenPolicyCache: NewSeenPolicyCache(),
	}

	fc := fetcher.ConfigForRepositoryBranch(context.Background(), nil, "testorg", "testrepo", "main")
	require.Error(t, fc.LoadError)
	assert.Equal(t, 1, calls)
}
