// Copyright 2021 FerretDB Inc.
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

package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
	"golang.org/x/exp/slices"

	"github.com/FerretDB/FerretDB/integration/shareddata"
	"github.com/FerretDB/FerretDB/internal/util/testutil"
)

// SetupOpts represents setup options.
//
// TODO Add option to use read-only user. https://github.com/FerretDB/FerretDB/issues/1025
type SetupOpts struct {
	// Database to use. If empty, temporary test-specific database is created and dropped after test.
	DatabaseName string

	// Collection to use. If empty, temporary test-specific collection is created and dropped after test.
	// Most tests should keep this empty.
	CollectionName string

	// Data providers. If empty, collection is not created.
	Providers []shareddata.Provider
}

// SetupResult represents setup results.
type SetupResult struct {
	Ctx        context.Context
	Collection *mongo.Collection
	MongoDBURI string
}

// IsUnixSocket returns true if MongoDB URI is a Unix socket.
func (s *SetupResult) IsUnixSocket(tb testing.TB) bool {
	tb.Helper()

	// we can't use a regular url.Parse because
	// MongoDB really wants Unix socket path in the host part of the URI
	opts := options.Client().ApplyURI(s.MongoDBURI)
	res := slices.ContainsFunc(opts.Hosts, func(host string) bool {
		return strings.Contains(host, "/")
	})

	tb.Logf("IsUnixSocket: %q - %v", s.MongoDBURI, res)

	return res
}

// SetupWithOpts setups the test according to given options.
func SetupWithOpts(tb testing.TB, opts *SetupOpts) *SetupResult {
	tb.Helper()

	startup()

	if opts == nil {
		opts = new(SetupOpts)
	}

	ctx, cancel := context.WithCancel(testutil.Ctx(tb))

	level := zap.NewAtomicLevelAt(zap.ErrorLevel)
	if *debugSetupF {
		level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}
	logger := testutil.Logger(tb, level)

	var uri string
	if *targetPortF == 0 {
		uri = setupListener(tb, ctx, logger)
	} else {
		uri = buildMongoDBURI(tb, ctx, &buildMongoDBURIOpts{
			hostPort: fmt.Sprintf("127.0.0.1:%d", *targetPortF),
			tls:      *targetTLSF,
		})
	}

	// register cleanup function after setupListener registers its own to preserve full logs
	tb.Cleanup(cancel)

	collection := setupCollection(tb, ctx, setupClient(tb, ctx, uri), opts)

	level.SetLevel(*logLevelF)

	return &SetupResult{
		Ctx:        ctx,
		Collection: collection,
		MongoDBURI: uri,
	}
}

// Setup setups a single collection for all compatible providers, if the are present.
func Setup(tb testing.TB, providers ...shareddata.Provider) (context.Context, *mongo.Collection) {
	tb.Helper()

	s := SetupWithOpts(tb, &SetupOpts{
		Providers: providers,
	})
	return s.Ctx, s.Collection
}

// setupCollection setups a single collection for all compatible providers, if they are present.
func setupCollection(tb testing.TB, ctx context.Context, client *mongo.Client, opts *SetupOpts) *mongo.Collection {
	tb.Helper()

	var ownDatabase bool
	databaseName := opts.DatabaseName
	if databaseName == "" {
		databaseName = testutil.DatabaseName(tb)
		ownDatabase = true
	}

	var ownCollection bool
	collectionName := opts.CollectionName
	if collectionName == "" {
		collectionName = testutil.CollectionName(tb)
		ownCollection = true
	}

	database := client.Database(databaseName)
	collection := database.Collection(collectionName)

	// drop remnants of the previous failed run
	_ = collection.Drop(ctx)
	if ownDatabase {
		_ = database.Drop(ctx)
	}

	var inserted bool
	for _, provider := range opts.Providers {
		if *targetPortF == 0 && !slices.Contains(provider.Handlers(), *handlerF) {
			tb.Logf(
				"Provider %q is not compatible with handler %q, skipping it.",
				provider.Name(), *handlerF,
			)

			continue
		}

		// if validators are set, create collection with them (otherwise collection will be created on first insert)
		if validators := provider.Validators(*handlerF, collectionName); len(validators) > 0 {
			var copts options.CreateCollectionOptions
			for key, value := range validators {
				copts.SetValidator(bson.D{{key, value}})
			}

			require.NoError(tb, database.CreateCollection(ctx, collectionName, &copts))
		}

		docs := shareddata.Docs(provider)
		require.NotEmpty(tb, docs)

		res, err := collection.InsertMany(ctx, docs)
		require.NoError(tb, err, "provider %q", provider.Name())
		require.Len(tb, res.InsertedIDs, len(docs))
		inserted = true
	}

	if len(opts.Providers) == 0 {
		tb.Logf("Collection %s.%s wasn't created because no providers were set.", databaseName, collectionName)
	} else {
		require.True(tb, inserted, "all providers were not compatible")
	}

	if ownCollection {
		// delete collection and (possibly) database unless test failed
		tb.Cleanup(func() {
			if tb.Failed() {
				tb.Logf("Keeping %s.%s for debugging.", databaseName, collectionName)
				return
			}

			err := collection.Drop(ctx)
			require.NoError(tb, err)

			if ownDatabase {
				err = database.Drop(ctx)
				require.NoError(tb, err)
			}
		})
	}

	return collection
}
