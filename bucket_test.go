package pail

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/evergreen-ci/pail/testutil"
	"github.com/evergreen-ci/utility"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type bucketTestCase struct {
	id   string
	test func(*testing.T, Bucket)
}

func TestBucket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	uuid := testutil.NewUUID()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	require.NoError(t, err)
	dbName := "pail-bucket-test"
	defer func() {
		require.NoError(t, client.Database(dbName).Drop(ctx))
	}()

	_, file, _, _ := runtime.Caller(0)
	tempdir := t.TempDir()
	require.NoError(t, err, os.MkdirAll(filepath.Join(tempdir, uuid), 0700))

	s3Credentials := CreateAWSStaticCredentials(os.Getenv("AWS_KEY"), os.Getenv("AWS_SECRET"), "")
	s3BucketName := "build-test-curator"
	s3Prefix := testutil.NewUUID() + "-"
	s3Region := "us-east-1"
	defer func() {
		require.NoError(t, testutil.CleanupS3Bucket(ctx, s3Credentials, s3BucketName, s3Prefix, s3Region))
	}()

	for _, impl := range []struct {
		name        string
		constructor func(*testing.T) Bucket
		tests       []bucketTestCase
	}{
		{
			name: "GridFS",
			constructor: func(t *testing.T) Bucket {
				require.NoError(t, client.Database(uuid).Drop(ctx))
				b, err := NewGridFSBucketWithClient(ctx, client, GridFSOptions{
					Name:     testutil.NewUUID(),
					Prefix:   testutil.NewUUID(),
					Database: dbName,
				})
				require.NoError(t, err)
				return b
			},
			tests: []bucketTestCase{
				{
					id: "VerifyBucketType",
					test: func(t *testing.T, b Bucket) {
						bucket, ok := b.(*gridfsBucket)
						require.True(t, ok)
						assert.NotNil(t, bucket)
					},
				},
			},
		},
		{
			name: "Local",
			constructor: func(t *testing.T) Bucket {
				path := filepath.Join(tempdir, uuid)
				require.NoError(t, os.MkdirAll(path, 0777))
				return &localFileSystem{path: path, prefix: testutil.NewUUID()}
			},
			tests: []bucketTestCase{
				{
					id: "VerifyBucketType",
					test: func(t *testing.T, b Bucket) {
						bucket, ok := b.(*localFileSystem)
						require.True(t, ok)
						assert.NotNil(t, bucket)
					},
				},
				{
					id: "PathDoesNotExist",
					test: func(t *testing.T, b Bucket) {
						bucket := b.(*localFileSystem)
						bucket.path = "foo"
						assert.Error(t, bucket.Check(ctx))
					},
				},
				{
					id: "WriterErrorFileName",
					test: func(t *testing.T, b Bucket) {
						_, err := b.Writer(ctx, "\x00")
						require.Error(t, err)
						assert.Contains(t, err.Error(), "opening file")
					},
				},
				{
					id: "ReaderErrorFileName",
					test: func(t *testing.T, b Bucket) {
						_, err := b.Reader(ctx, "\x00")
						require.Error(t, err)
						assert.Contains(t, err.Error(), "opening file")
					},
				},
				{
					id: "CopyErrorFileNameFrom",
					test: func(t *testing.T, b Bucket) {
						options := CopyOptions{
							SourceKey:         "\x00",
							DestinationKey:    "foo",
							DestinationBucket: b,
						}
						err := b.Copy(ctx, options)
						require.Error(t, err)
						assert.Contains(t, err.Error(), "opening file")
					},
				},
				{
					id: "CopyErrorFileNameTo",
					test: func(t *testing.T, b Bucket) {
						fn := filepath.Base(file)
						err := b.Upload(ctx, "foo", fn)
						require.NoError(t, err)

						options := CopyOptions{
							SourceKey:         "foo",
							DestinationKey:    "\x00",
							DestinationBucket: b,
						}
						err = b.Copy(ctx, options)
						require.Error(t, err)
						assert.Contains(t, err.Error(), "opening file")
					},
				},
				{
					id: "PutErrorFileName",
					test: func(t *testing.T, b Bucket) {
						err := b.Put(ctx, "\x00", nil)
						require.Error(t, err)
						assert.Contains(t, err.Error(), "opening file")
					},
				},
				{
					id: "PutErrorReader",
					test: func(t *testing.T, b Bucket) {
						err := b.Put(ctx, "foo", &brokenWriter{})
						require.Error(t, err)
						assert.Contains(t, err.Error(), "copying data to file")
					},
				},
				{
					id: "WriterErrorDirectoryName",
					test: func(t *testing.T, b Bucket) {
						bucket := b.(*localFileSystem)
						bucket.path = "\x00"
						_, err := b.Writer(ctx, "foo")
						require.Error(t, err)
						assert.Contains(t, err.Error(), "creating base directories")
					},
				},
				{
					id: "PullErrorsContext",
					test: func(t *testing.T, b Bucket) {
						tctx, cancel := context.WithCancel(ctx)
						cancel()
						bucket := b.(*localFileSystem)
						bucket.path = ""
						bucket.prefix = ""
						opts := SyncOptions{Remote: filepath.Dir(file)}
						err := b.Pull(tctx, opts)
						assert.Error(t, err)
					},
				},
				{
					id: "PushErrorsContext",
					test: func(t *testing.T, b Bucket) {
						tctx, cancel := context.WithCancel(ctx)
						cancel()
						opts := SyncOptions{Local: filepath.Dir(file)}
						err := b.Push(tctx, opts)
						assert.Error(t, err)
					},
				},
			},
		},
		{
			name: "LocalSlashSeparator",
			constructor: func(t *testing.T) Bucket {
				path := filepath.Join(tempdir, uuid)
				require.NoError(t, os.MkdirAll(path, 0777))
				return &localFileSystem{path: path, prefix: testutil.NewUUID(), useSlash: true}
			},
		},
		{
			name: "S3Bucket",
			constructor: func(t *testing.T) Bucket {
				s3Options := S3Options{
					Credentials: s3Credentials,
					Region:      s3Region,
					Name:        s3BucketName,
					Prefix:      s3Prefix + testutil.NewUUID(),
					MaxRetries:  aws.Int(20),
				}
				b, err := NewS3Bucket(ctx, s3Options)
				require.NoError(t, err)
				return b
			},
			tests: getS3SmallBucketTests(ctx, tempdir, s3Credentials, s3BucketName, s3Prefix, s3Region),
		},
		{
			name: "S3BucketChecksums",
			constructor: func(t *testing.T) Bucket {
				s3Options := S3Options{
					Credentials:            s3Credentials,
					Region:                 s3Region,
					Name:                   s3BucketName,
					Prefix:                 s3Prefix + testutil.NewUUID(),
					MaxRetries:             aws.Int(20),
					UseSingleFileChecksums: true,
				}
				b, err := NewS3Bucket(ctx, s3Options)
				require.NoError(t, err)
				return b
			},
			tests: getS3SmallBucketTests(ctx, tempdir, s3Credentials, s3BucketName, s3Prefix, s3Region),
		},
		{
			name: "ParallelLocal",
			constructor: func(t *testing.T) Bucket {
				t.Skip()
				path := filepath.Join(tempdir, uuid, testutil.NewUUID())
				require.NoError(t, os.MkdirAll(path, 0777))
				bucket := &localFileSystem{path: path}

				b, err := NewParallelSyncBucket(ParallelBucketOptions{Workers: runtime.NumCPU()}, bucket)
				require.NoError(t, err)
				return b
			},
		},
		{
			name: "ParallelS3Bucket",
			constructor: func(t *testing.T) Bucket {
				s3Options := S3Options{
					Credentials:            s3Credentials,
					Region:                 s3Region,
					Name:                   s3BucketName,
					Prefix:                 s3Prefix + testutil.NewUUID(),
					MaxRetries:             aws.Int(20),
					UseSingleFileChecksums: true,
				}
				b, err := NewS3Bucket(ctx, s3Options)
				require.NoError(t, err)

				b, err = NewParallelSyncBucket(ParallelBucketOptions{Workers: runtime.NumCPU()}, b)
				require.NoError(t, err)
				return b
			},
		},
		{
			name: "S3MultiPartBucket",
			constructor: func(t *testing.T) Bucket {
				s3Options := S3Options{
					Credentials: s3Credentials,
					Region:      s3Region,
					Name:        s3BucketName,
					Prefix:      s3Prefix + testutil.NewUUID(),
					MaxRetries:  aws.Int(20),
				}
				b, err := NewS3MultiPartBucket(ctx, s3Options)
				require.NoError(t, err)
				return b
			},
			tests: getS3LargeBucketTests(ctx, tempdir, s3Credentials, s3BucketName, s3Prefix, s3Region),
		},
		{
			name: "S3MultiPartBucketChecksum",
			constructor: func(t *testing.T) Bucket {
				s3Options := S3Options{
					Credentials:            s3Credentials,
					Region:                 s3Region,
					Name:                   s3BucketName,
					Prefix:                 s3Prefix + testutil.NewUUID(),
					MaxRetries:             aws.Int(20),
					UseSingleFileChecksums: true,
				}
				b, err := NewS3MultiPartBucket(ctx, s3Options)
				require.NoError(t, err)
				return b
			},
			tests: getS3LargeBucketTests(ctx, tempdir, s3Credentials, s3BucketName, s3Prefix, s3Region),
		},
	} {
		t.Run(impl.name, func(t *testing.T) {
			// Only test the local implementation using the slash
			// ('/') separator where the OS specific separator is
			// different.
			if impl.name == "LocalSlashSeparator" && runtime.GOOS != "windows" {
				t.Skip()
			}

			for _, test := range impl.tests {
				t.Run(test.id, func(t *testing.T) {
					bucket := impl.constructor(t)
					test.test(t, bucket)
				})
			}
			t.Run("ValidateFixture", func(t *testing.T) {
				assert.NotNil(t, impl.constructor(t))
			})
			t.Run("CheckIsValid", func(t *testing.T) {
				assert.NoError(t, impl.constructor(t).Check(ctx))
			})
			t.Run("ListIsEmpty", func(t *testing.T) {
				bucket := impl.constructor(t)
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.False(t, iter.Next(ctx))
				assert.Nil(t, iter.Item())
				assert.NoError(t, iter.Err())
			})
			t.Run("ListErrorsWithCanceledContext", func(t *testing.T) {
				bucket := impl.constructor(t)
				tctx, cancel := context.WithCancel(ctx)
				cancel()
				iter, err := bucket.List(tctx, "")
				assert.Error(t, err)
				assert.Nil(t, iter)
			})
			t.Run("WriteOneFile", func(t *testing.T) {
				bucket := impl.constructor(t)
				assert.NoError(t, writeDataToFile(ctx, bucket, testutil.NewUUID(), "hello world!"))

				// Dry run does not write.
				setDryRun(bucket, true)
				assert.NoError(t, writeDataToFile(ctx, bucket, testutil.NewUUID(), "hello world!"))

				// Check that only one key exists in the
				// iterator.
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.True(t, iter.Next(ctx))
				assert.False(t, iter.Next(ctx))
				assert.NoError(t, iter.Err())
			})
			t.Run("RemoveOneFile", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := testutil.NewUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, key, "hello world!"))

				// Dry run does not remove anything.
				setDryRun(bucket, true)
				assert.NoError(t, bucket.Remove(ctx, key))
				setDryRun(bucket, false)

				// Check that it exists in the iterator.
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.True(t, iter.Next(ctx))
				assert.False(t, iter.Next(ctx))
				assert.NoError(t, iter.Err())

				assert.NoError(t, bucket.Remove(ctx, key))
				iter, err = bucket.List(ctx, "")
				require.NoError(t, err)
				assert.False(t, iter.Next(ctx))
				assert.Nil(t, iter.Item())
				assert.NoError(t, iter.Err())
			})
			t.Run("RemoveManyFiles", func(t *testing.T) {
				data := map[string]string{}
				keys := []string{}
				deleteData := map[string]string{}
				deleteKeys := []string{}
				for i := 0; i < 20; i++ {
					key := testutil.NewUUID()
					data[key] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					keys = append(keys, key)
				}
				assert.Len(t, keys, 20)
				for i := 0; i < 20; i++ {
					key := testutil.NewUUID()
					deleteData[key] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					deleteKeys = append(deleteKeys, key)
				}
				assert.Len(t, deleteKeys, 20)

				bucket := impl.constructor(t)
				for k, v := range data {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}
				for k, v := range deleteData {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}

				// Smaller s3 batch sizes for testing.
				switch i := bucket.(type) {
				case *s3BucketSmall:
					i.batchSize = 20
				case *s3BucketLarge:
					i.batchSize = 20
				}

				// Check keys are in bucket.
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				for iter.Next(ctx) {
					assert.NoError(t, iter.Err())
					require.NotNil(t, iter.Item())
					_, ok1 := data[iter.Item().Name()]
					_, ok2 := deleteData[iter.Item().Name()]
					assert.True(t, ok1 || ok2)
				}

				assert.NoError(t, bucket.RemoveMany(ctx, deleteKeys...))
				iter, err = bucket.List(ctx, "")
				require.NoError(t, err)
				for iter.Next(ctx) {
					assert.NoError(t, iter.Err())
					require.NotNil(t, iter.Item())
					_, ok := data[iter.Item().Name()]
					assert.True(t, ok)
					_, ok = deleteData[iter.Item().Name()]
					assert.False(t, ok)
				}

			})
			t.Run("RemovePrefix", func(t *testing.T) {
				data := map[string]string{}
				keys := []string{}
				deleteData := map[string]string{}
				deleteKeys := []string{}
				prefix := testutil.NewUUID()
				for i := 0; i < 5; i++ {
					key := testutil.NewUUID()
					data[key] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					keys = append(keys, key)
				}
				assert.Len(t, keys, 5)
				for i := 0; i < 5; i++ {
					key := prefix + testutil.NewUUID()
					deleteData[key] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					deleteKeys = append(deleteKeys, key)
				}
				assert.Len(t, deleteKeys, 5)

				bucket := impl.constructor(t)
				for k, v := range data {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}
				for k, v := range deleteData {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}

				// Check keys are in bucket.
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				for iter.Next(ctx) {
					assert.NoError(t, iter.Err())
					require.NotNil(t, iter.Item())
					_, ok1 := data[iter.Item().Name()]
					_, ok2 := deleteData[iter.Item().Name()]
					assert.True(t, ok1 || ok2)
				}

				assert.NoError(t, bucket.RemoveMany(ctx, deleteKeys...))
				iter, err = bucket.List(ctx, "")
				require.NoError(t, err)
				for iter.Next(ctx) {
					assert.NoError(t, iter.Err())
					require.NotNil(t, iter.Item())
					_, ok := data[iter.Item().Name()]
					assert.True(t, ok)
					_, ok = deleteData[iter.Item().Name()]
					assert.False(t, ok)
				}
			})
			t.Run("RemoveMatching", func(t *testing.T) {
				data := map[string]string{}
				keys := []string{}
				deleteData := map[string]string{}
				deleteKeys := []string{}
				postfix := testutil.NewUUID()
				for i := 0; i < 5; i++ {
					key := testutil.NewUUID()
					data[key] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					keys = append(keys, key)
				}
				assert.Len(t, keys, 5)
				for i := 0; i < 5; i++ {
					key := testutil.NewUUID() + postfix
					deleteData[key] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					deleteKeys = append(deleteKeys, key)
				}
				assert.Len(t, deleteKeys, 5)

				bucket := impl.constructor(t)
				for k, v := range data {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}
				for k, v := range deleteData {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}

				// Check keys are in bucket.
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				for iter.Next(ctx) {
					assert.NoError(t, iter.Err())
					require.NotNil(t, iter.Item())
					_, ok1 := data[iter.Item().Name()]
					_, ok2 := deleteData[iter.Item().Name()]
					assert.True(t, ok1 || ok2)
				}

				assert.NoError(t, bucket.RemoveMatching(ctx, ".*"+postfix))
				iter, err = bucket.List(ctx, "")
				require.NoError(t, err)
				for iter.Next(ctx) {
					assert.NoError(t, iter.Err())
					require.NotNil(t, iter.Item())
					_, ok := data[iter.Item().Name()]
					assert.True(t, ok)
					_, ok = deleteData[iter.Item().Name()]
					assert.False(t, ok)
				}
			})
			t.Run("RemoveMatchingInvalidExpression", func(t *testing.T) {
				bucket := impl.constructor(t)
				assert.Error(t, bucket.RemoveMatching(ctx, "["))
			})
			t.Run("ReadWriteRoundTripSimple", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := testutil.NewUUID()
				payload := "hello world!"
				require.NoError(t, writeDataToFile(ctx, bucket, key, payload))

				data, err := readDataFromFile(ctx, bucket, key)
				assert.NoError(t, err)
				assert.Equal(t, data, payload)
			})

			t.Run("GetRetrievesData", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := testutil.NewUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, key, "hello world!"))

				reader, err := bucket.Get(ctx, key)
				require.NoError(t, err)
				data, err := ioutil.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, "hello world!", string(data))

				// Dry run bucket also retrieves data.
				setDryRun(bucket, true)
				reader, err = bucket.Get(ctx, key)
				require.NoError(t, err)
				data, err = ioutil.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, "hello world!", string(data))
			})
			t.Run("PutSavesFiles", func(t *testing.T) {
				const contents = "check data"
				bucket := impl.constructor(t)
				key := testutil.NewUUID()
				assert.NoError(t, bucket.Put(ctx, key, bytes.NewBuffer([]byte(contents))))

				reader, err := bucket.Get(ctx, key)
				require.NoError(t, err)
				data, err := ioutil.ReadAll(reader)
				require.NoError(t, err)
				assert.Equal(t, contents, string(data))
			})
			t.Run("PutWithDryRunDoesNotSaveFiles", func(t *testing.T) {
				const contents = "check data"
				bucket := impl.constructor(t)
				setDryRun(bucket, true)
				key := testutil.NewUUID()
				assert.NoError(t, bucket.Put(ctx, key, bytes.NewBuffer([]byte(contents))))

				_, err := bucket.Get(ctx, key)
				assert.Error(t, err)
			})
			t.Run("CopyDuplicatesData", func(t *testing.T) {
				const contents = "this one"
				bucket := impl.constructor(t)
				keyOne := testutil.NewUUID()
				keyTwo := testutil.NewUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, keyOne, contents))
				options := CopyOptions{
					SourceKey:         keyOne,
					DestinationKey:    keyTwo,
					DestinationBucket: bucket,
				}
				assert.NoError(t, bucket.Copy(ctx, options))
				data, err := readDataFromFile(ctx, bucket, keyTwo)
				require.NoError(t, err)
				assert.Equal(t, contents, data)
			})
			t.Run("CopyDoesNotDuplicateDataToDryRunBucket", func(t *testing.T) {
				const contents = "this one"
				bucket := impl.constructor(t)
				dryRunBucket := impl.constructor(t)
				setDryRun(dryRunBucket, true)
				keyOne := testutil.NewUUID()
				keyTwo := testutil.NewUUID()
				assert.NoError(t, writeDataToFile(ctx, bucket, keyOne, contents))
				options := CopyOptions{
					SourceKey:         keyOne,
					DestinationKey:    keyTwo,
					DestinationBucket: dryRunBucket,
				}
				assert.NoError(t, bucket.Copy(ctx, options))
				_, err := dryRunBucket.Get(ctx, keyTwo)
				assert.Error(t, err)
			})
			t.Run("CopyDuplicatesDataFromDryRunBucket", func(t *testing.T) {
				const contents = "this one"
				bucket := impl.constructor(t)
				dryRunBucket := impl.constructor(t)
				keyOne := testutil.NewUUID()
				keyTwo := testutil.NewUUID()
				assert.NoError(t, writeDataToFile(ctx, dryRunBucket, keyOne, contents))
				setDryRun(dryRunBucket, true)
				options := CopyOptions{
					SourceKey:         keyOne,
					DestinationKey:    keyTwo,
					DestinationBucket: bucket,
				}
				assert.NoError(t, dryRunBucket.Copy(ctx, options))
				data, err := readDataFromFile(ctx, bucket, keyTwo)
				require.NoError(t, err)
				assert.Equal(t, contents, data)
			})
			t.Run("CopyDuplicatesToDifferentBucket", func(t *testing.T) {
				const contents = "this one"
				srcBucket := impl.constructor(t)
				destBucket := impl.constructor(t)
				keyOne := testutil.NewUUID()
				keyTwo := testutil.NewUUID()
				assert.NoError(t, writeDataToFile(ctx, srcBucket, keyOne, contents))
				options := CopyOptions{
					SourceKey:         keyOne,
					DestinationKey:    keyTwo,
					DestinationBucket: destBucket,
				}
				assert.NoError(t, srcBucket.Copy(ctx, options))
				data, err := readDataFromFile(ctx, destBucket, keyTwo)
				require.NoError(t, err)
				assert.Equal(t, contents, data)
			})
			t.Run("DownloadWritesFileToDisk", func(t *testing.T) {
				const contents = "in the file"
				bucket := impl.constructor(t)
				key := testutil.NewUUID()
				path := filepath.Join(tempdir, uuid, key)
				assert.NoError(t, writeDataToFile(ctx, bucket, key, contents))

				_, err := os.Stat(path)
				require.True(t, os.IsNotExist(err))
				require.NoError(t, bucket.Download(ctx, key, path))
				_, err = os.Stat(path)
				require.False(t, os.IsNotExist(err))

				data, err := ioutil.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, contents, string(data))

				// writes file to disk with dry run bucket
				setDryRun(bucket, true)
				path = filepath.Join(tempdir, uuid, testutil.NewUUID())
				_, err = os.Stat(path)
				require.True(t, os.IsNotExist(err))
				require.NoError(t, bucket.Download(ctx, key, path))
				_, err = os.Stat(path)
				require.False(t, os.IsNotExist(err))

				data, err = ioutil.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, contents, string(data))
			})
			t.Run("ListRespectsPrefixes", func(t *testing.T) {
				bucket := impl.constructor(t)
				key := testutil.NewUUID()

				assert.NoError(t, writeDataToFile(ctx, bucket, key, "foo/bar"))

				// There's one thing in the iterator with the
				// correct prefix.
				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				assert.True(t, iter.Next(ctx))
				assert.False(t, iter.Next(ctx))
				assert.NoError(t, iter.Err())

				// There's nothing in the iterator with a
				// prefix.
				iter, err = bucket.List(ctx, "bar")
				require.NoError(t, err)
				assert.False(t, iter.Next(ctx))
				assert.Nil(t, iter.Item())
				assert.NoError(t, iter.Err())
			})
			t.Run("ListIteratesLexicographically", func(t *testing.T) {
				bucket := impl.constructor(t)
				keys := []string{
					"0file",
					"Afile",
					"Zfile",
					"afile1",
					"cfile",
					"zfile1",
				}
				// Insert keys in reverse order because some
				// underlying bucket stores may iterate in
				// insert order by default.
				for i := len(keys) - 1; i >= 0; i-- {
					assert.NoError(t, writeDataToFile(ctx, bucket, keys[i], "foo/bar"))
				}

				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)

				var listedKeys []string
				for iter.Next(ctx) {
					listedKeys = append(listedKeys, iter.Item().Name())
				}
				require.NoError(t, iter.Err())
				assert.Equal(t, keys, listedKeys)
			})
			t.Run("RoundTripManyFiles", func(t *testing.T) {
				data := map[string]string{}
				for i := 0; i < 3; i++ {
					data[testutil.NewUUID()] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
				}

				bucket := impl.constructor(t)
				for k, v := range data {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}

				iter, err := bucket.List(ctx, "")
				require.NoError(t, err)
				count := 0
				for iter.Next(ctx) {
					count++
					item := iter.Item()
					require.NotNil(t, item)

					key := item.Name()
					_, ok := data[key]
					require.True(t, ok)
					assert.NotZero(t, item.Bucket())

					reader, err := item.Get(ctx)
					require.NoError(t, err)
					require.NotNil(t, reader)
					out, err := ioutil.ReadAll(reader)
					assert.NoError(t, err)
					assert.NoError(t, reader.Close())
					assert.Equal(t, string(out), data[item.Name()])
				}
				assert.NoError(t, iter.Err())
				assert.Equal(t, len(data), count)
			})
			t.Run("PullFromBucket", func(t *testing.T) {
				data := map[string]string{}
				const numFiles = 50
				for i := 0; i < numFiles; i++ {
					data[testutil.NewUUID()] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
				}

				bucket := impl.constructor(t)
				for k, v := range data {
					require.NoError(t, writeDataToFile(ctx, bucket, k, v))
				}

				t.Run("BasicPull", func(t *testing.T) {
					mirror := filepath.Join(tempdir, "basic-pull", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0700))
					opts := SyncOptions{Local: mirror}
					assert.NoError(t, bucket.Pull(ctx, opts))
					files, err := walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					require.Len(t, files, numFiles)

					for _, fn := range files {
						_, ok := data[filepath.Base(fn)]
						require.True(t, ok)
					}
				})
				t.Run("DryRunBucketPulls", func(t *testing.T) {
					setDryRun(bucket, true)
					mirror := filepath.Join(tempdir, "pull-dry-run", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0700))
					opts := SyncOptions{Local: mirror}
					assert.NoError(t, bucket.Pull(ctx, opts))
					files, err := walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					require.Len(t, files, numFiles)

					for _, fn := range files {
						_, ok := data[filepath.Base(fn)]
						require.True(t, ok)
					}
					setDryRun(bucket, false)
				})
				t.Run("PullWithExcludes", func(t *testing.T) {
					require.NoError(t, writeDataToFile(ctx, bucket, "python.py", "exclude"))
					require.NoError(t, writeDataToFile(ctx, bucket, "python2.py", "exclude2"))

					mirror := filepath.Join(tempdir, "pull-not-excludes", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0700))
					opts := SyncOptions{Local: mirror}
					assert.NoError(t, bucket.Pull(ctx, opts))
					files, err := walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					require.Len(t, files, numFiles+2)

					for _, fn := range files {
						_, ok := data[filepath.Base(fn)]
						if !ok {
							ok = filepath.Base(fn) == "python.py" || filepath.Base(fn) == "python2.py"
						}
						require.True(t, ok)
					}

					mirror = filepath.Join(tempdir, "pull-excludes", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0700))
					opts.Local = mirror
					opts.Exclude = ".*\\.py"
					assert.NoError(t, bucket.Pull(ctx, opts))
					files, err = walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					require.Len(t, files, numFiles)

					for _, fn := range files {
						_, ok := data[filepath.Base(fn)]
						require.True(t, ok)
					}

					require.NoError(t, bucket.Remove(ctx, "python.py"))
					require.NoError(t, bucket.Remove(ctx, "python2.py"))
				})
				t.Run("DeleteOnSync", func(t *testing.T) {
					setDeleteOnSync(bucket, true)

					// Dry run bucket does not delete.
					mirror := filepath.Join(tempdir, "pull-delete", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0700))
					require.NoError(t, writeDataToDisk(mirror, "delete1", "should be deleted"))
					require.NoError(t, writeDataToDisk(mirror, "delete2", "this should also be deleted"))
					setDryRun(bucket, true)
					opts := SyncOptions{Local: mirror}
					require.NoError(t, bucket.Pull(ctx, opts))
					files, err := walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					require.Len(t, files, numFiles+2)
					setDryRun(bucket, false)

					// With out dry run set.
					mirror = filepath.Join(tempdir, "pull-delete", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0700))
					require.NoError(t, writeDataToDisk(mirror, "delete1", "should be deleted"))
					require.NoError(t, writeDataToDisk(mirror, "delete2", "this should also be deleted"))
					opts.Local = mirror
					assert.NoError(t, bucket.Pull(ctx, opts))
					files, err = walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					assert.Len(t, files, numFiles)
					setDeleteOnSync(bucket, false)
				})
				t.Run("LargePull", func(t *testing.T) {
					prefix := testutil.NewUUID()
					largeData := map[string]string{}
					for i := 0; i < 1050; i++ {
						largeData[testutil.NewUUID()] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					}
					for k, v := range largeData {
						require.NoError(t, writeDataToFile(ctx, bucket, bucket.Join(prefix, k), v))
					}

					mirror := filepath.Join(tempdir, "pull-large", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0700))

					opts := SyncOptions{Local: mirror, Remote: prefix}
					assert.NoError(t, bucket.Pull(ctx, opts))
					files, err := walkLocalTree(ctx, mirror)
					require.NoError(t, err)
					assert.Len(t, files, len(largeData))

					for _, fn := range files {
						_, ok := largeData[fn]
						require.True(t, ok)
					}
				})
			})
			t.Run("PushToBucket", func(t *testing.T) {
				prefix := filepath.Join(tempdir, testutil.NewUUID())
				filenames := map[string]bool{}
				const numFiles = 50
				for i := 0; i < numFiles; i++ {
					// Add an additional prefix here to
					// ensure that we correctly replace the
					// local path separator with that of
					// the bucket.
					fnPrefix := testutil.NewUUID()
					fn := testutil.NewUUID()
					filenames[filepath.Join(fnPrefix, fn)] = true
					require.NoError(t, writeDataToDisk(filepath.Join(prefix, fnPrefix),
						fn, strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")))
				}
				bucket := impl.constructor(t)
				t.Run("NoPrefix", func(t *testing.T) {
					opts := SyncOptions{Local: prefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					iter, err := bucket.List(ctx, "")
					require.NoError(t, err)
					counter := 0
					for iter.Next(ctx) {
						key := iter.Item().Name()
						assert.Equal(t, bucket.Join(key), key)
						require.True(t, filenames[filepath.FromSlash(key)])
						counter++
					}
					assert.NoError(t, iter.Err())
					assert.Equal(t, numFiles, counter)
				})
				t.Run("ShortPrefix", func(t *testing.T) {
					// Using a different path separator
					// than that of the bucket should not
					// matter.
					remotePrefix := filepath.Join("foo", "baz")
					opts := SyncOptions{Local: prefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					iter, err := bucket.List(ctx, remotePrefix)
					require.NoError(t, err)
					counter := 0
					for iter.Next(ctx) {
						key := iter.Item().Name()
						assert.Equal(t, bucket.Join(key), key)
						fn, err := filepath.Rel(remotePrefix, key)
						require.NoError(t, err)
						require.True(t, filenames[fn])
						counter++
					}
					assert.NoError(t, iter.Err())
					assert.Equal(t, numFiles, counter)
				})
				t.Run("DryRunBucketDoesNotPush", func(t *testing.T) {
					remotePrefix := "bar"
					setDryRun(bucket, true)
					opts := SyncOptions{Local: prefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					iter, err := bucket.List(ctx, remotePrefix)
					require.NoError(t, err)
					counter := 0
					for iter.Next(ctx) {
						counter++
					}
					assert.NoError(t, iter.Err())
					assert.Equal(t, 0, counter)

					setDryRun(bucket, false)
				})
				t.Run("PushWithExcludes", func(t *testing.T) {
					require.NoError(t, writeDataToDisk(prefix, "python.py", "exclude"))
					require.NoError(t, writeDataToDisk(prefix, "python2.py", "exclude2"))

					remotePrefix := "not_excludes"
					opts := SyncOptions{Local: prefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))
					iter, err := bucket.List(ctx, remotePrefix)
					require.NoError(t, err)
					counter := 0
					for iter.Next(ctx) {
						var fn string
						fn, err = filepath.Rel(remotePrefix, iter.Item().Name())
						require.NoError(t, err)
						ok := filenames[fn]
						if !ok {
							ok = fn == "python.py" || fn == "python2.py"
						}
						require.True(t, ok)
						counter++
					}
					assert.NoError(t, iter.Err())
					assert.Equal(t, numFiles+2, counter)

					remotePrefix = "excludes"
					opts.Remote = remotePrefix
					opts.Exclude = ".*\\.py"
					assert.NoError(t, bucket.Push(ctx, opts))
					iter, err = bucket.List(ctx, remotePrefix)
					require.NoError(t, err)
					counter = 0
					for iter.Next(ctx) {
						fn, err := filepath.Rel(remotePrefix, iter.Item().Name())
						require.NoError(t, err)
						require.True(t, filenames[fn])
						counter++
					}
					assert.NoError(t, iter.Err())
					assert.Equal(t, numFiles, counter)

					require.NoError(t, os.RemoveAll(filepath.Join(prefix, "python.py")))
					require.NoError(t, os.RemoveAll(filepath.Join(prefix, "python2.py")))
				})
				t.Run("DeleteOnSync", func(t *testing.T) {
					setDeleteOnSync(bucket, true)

					contents := []byte("should be deleted")
					require.NoError(t, bucket.Put(ctx, bucket.Join("baz", "delete1"), bytes.NewBuffer(contents)))
					contents = []byte("this should also be deleted")
					require.NoError(t, bucket.Put(ctx, bucket.Join("baz", "delete2"), bytes.NewBuffer(contents)))

					// Dry run bucket does not push or
					// delete.
					setDryRun(bucket, true)
					opts := SyncOptions{Local: prefix, Remote: "baz"}
					assert.NoError(t, bucket.Push(ctx, opts))
					iter, err := bucket.List(ctx, "baz")
					require.NoError(t, err)
					count := 0
					for iter.Next(ctx) {
						require.NotNil(t, iter.Item())
						count++
					}
					assert.Equal(t, 2, count)
					setDryRun(bucket, false)

					assert.NoError(t, bucket.Push(ctx, opts))
					iter, err = bucket.List(ctx, "baz")
					require.NoError(t, err)
					count = 0
					for iter.Next(ctx) {
						require.NotNil(t, iter.Item())
						count++
					}
					assert.Equal(t, numFiles, count)

					setDeleteOnSync(bucket, false)
				})
			})
			t.Run("UploadWithBadFileName", func(t *testing.T) {
				bucket := impl.constructor(t)
				err := bucket.Upload(ctx, "key", "foo\x00bar")
				require.Error(t, err)
			})
			t.Run("DownloadWithBadFileName", func(t *testing.T) {
				bucket := impl.constructor(t)
				// This breaks the convention in the tests
				// where we use the null terminator ('\x00') to
				// simulate an invalid key name because it
				// causes Download to hang on newer versions
				// of the AWS SDK.
				err := bucket.Download(ctx, "fileIWant", filepath.Join(tempdir, "loc"))
				assert.Error(t, err)
			})
			t.Run("DownloadBadDirectory", func(t *testing.T) {
				bucket := impl.constructor(t)
				fn := filepath.Base(file)
				err := bucket.Upload(ctx, "key", fn)
				require.NoError(t, err)

				err = bucket.Download(ctx, "key", "location-\x00/key-name")
				require.Error(t, err)
			})
			t.Run("DownloadToBadFileName", func(t *testing.T) {
				bucket := impl.constructor(t)
				fn := filepath.Base(file)
				err := bucket.Upload(ctx, "key", fn)
				require.NoError(t, err)

				err = bucket.Download(ctx, "key", "location-\x00-key-name")
				require.Error(t, err)
			})
			t.Run("Exists", func(t *testing.T) {
				bucket := impl.constructor(t)
				require.NoError(t, writeDataToFile(ctx, bucket, "key0", "data for key0"))
				require.NoError(t, writeDataToFile(ctx, bucket, "key1", "data for key1"))

				t.Run("KeyDNE", func(t *testing.T) {
					exists, err := bucket.Exists(ctx, "DNE")
					require.NoError(t, err)
					assert.False(t, exists)
				})
				t.Run("KeyExists", func(t *testing.T) {
					exists, err := bucket.Exists(ctx, "key0")
					require.NoError(t, err)
					assert.True(t, exists)
				})
			})
		})
	}
}

func writeDataToDisk(prefix, key, data string) error {
	if err := os.MkdirAll(prefix, 0700); err != nil {
		return errors.WithStack(err)
	}
	path := filepath.Join(prefix, key)
	return errors.WithStack(ioutil.WriteFile(path, []byte(data), 0600))
}

func writeDataToFile(ctx context.Context, bucket Bucket, key, data string) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writer, err := bucket.Writer(wctx, key)
	if err != nil {
		return errors.WithStack(err)
	}
	_, err = writer.Write([]byte(data))
	if err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(writer.Close())
}

func TestS3ArchiveBucket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tempdir, err := ioutil.TempDir("", "pail-bucket-test")
	require.NoError(t, err)
	defer func() { require.NoError(t, os.RemoveAll(tempdir)) }()

	s3Credentials := CreateAWSStaticCredentials(os.Getenv("AWS_KEY"), os.Getenv("AWS_SECRET"), "")
	s3BucketName := "build-test-curator"
	s3Prefix := testutil.NewUUID() + "-"
	s3Region := "us-east-1"
	defer func() {
		require.NoError(t, testutil.CleanupS3Bucket(ctx, s3Credentials, s3BucketName, s3Prefix, s3Region))
	}()

	for _, impl := range []struct {
		name        string
		constructor func(*testing.T) *s3ArchiveBucket
	}{
		{
			name: "S3Archive",
			constructor: func(t *testing.T) *s3ArchiveBucket {
				s3Options := S3Options{
					Credentials: s3Credentials,
					Region:      s3Region,
					Name:        s3BucketName,
					Prefix:      s3Prefix + testutil.NewUUID(),
					MaxRetries:  aws.Int(20),
				}
				bucket, err := NewS3ArchiveBucket(ctx, s3Options)
				require.NoError(t, err)
				archiveBucket, ok := bucket.(*s3ArchiveBucket)
				require.True(t, ok)
				return archiveBucket
			},
		},
	} {
		t.Run("ValidateFixture", func(t *testing.T) {
			assert.NotNil(t, impl.constructor(t))
		})
		t.Run("ReadWriteArchiveRoundTripSimple", func(t *testing.T) {
			bucket := impl.constructor(t)
			prefix := testutil.NewUUID()
			payload := map[string]string{"my_file.txt": "hello world!"}
			require.NoError(t, writeDataToArchive(ctx, bucket, prefix, payload))

			data, err := readDataFromArchive(ctx, bucket, prefix)
			require.NoError(t, err)
			assert.Equal(t, payload, data)
		})
		t.Run("PullFromBucket", func(t *testing.T) {
			for testName, testCase := range map[string]func(t *testing.T, bucket *s3ArchiveBucket, data map[string]string){
				"BasicPull": func(t *testing.T, bucket *s3ArchiveBucket, data map[string]string) {
					mirror := filepath.Join(tempdir, testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))

					opts := SyncOptions{Local: mirror}
					require.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, data))
				},
				"DryRunBucketPulls": func(t *testing.T, bucket *s3ArchiveBucket, data map[string]string) {
					setDryRun(bucket.s3BucketLarge, true)
					mirror := filepath.Join(tempdir, testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))

					opts := SyncOptions{Local: mirror}
					assert.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, data))
				},
				"PullWithExcludes": func(t *testing.T, bucket *s3ArchiveBucket, data map[string]string) {
					dataWithExcluded := map[string]string{}
					for k, v := range data {
						dataWithExcluded[k] = v
					}
					dataWithExcluded["python.py"] = "exclude"
					dataWithExcluded["python2.py"] = "exclude2"
					require.NoError(t, writeDataToArchive(ctx, bucket, "", dataWithExcluded))

					mirror := filepath.Join(tempdir, "not_excludes", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))
					opts := SyncOptions{Local: mirror}
					assert.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, dataWithExcluded))

					mirror = filepath.Join(tempdir, "excludes", testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))
					opts = SyncOptions{Local: mirror, Exclude: ".*\\.py"}
					assert.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, data))
				},
			} {
				t.Run(testName, func(t *testing.T) {
					bucket := impl.constructor(t)
					data := map[string]string{}
					for i := 0; i < 50; i++ {
						data[testutil.NewUUID()] = strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
					}
					require.NoError(t, writeDataToArchive(ctx, bucket, "", data))
					testCase(t, bucket, data)
				})
			}
		})
		t.Run("PushToBucket", func(t *testing.T) {
			for testName, testCase := range map[string]func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string){
				"NoPrefix": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					opts := SyncOptions{Local: localPrefix}
					require.NoError(t, bucket.Push(ctx, opts))

					s3Data, err := readDataFromArchive(ctx, bucket, "")
					require.NoError(t, err)
					assert.Equal(t, data, s3Data)
				},
				"ShortPrefix": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					remotePrefix := testutil.NewUUID()
					opts := SyncOptions{Local: localPrefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					s3Data, err := readDataFromArchive(ctx, bucket, remotePrefix)
					require.NoError(t, err)
					assert.Equal(t, data, s3Data)
				},
				"DryRunBucketDoesNotPush": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					setDryRun(bucket.s3BucketLarge, true)
					remotePrefix := testutil.NewUUID()
					opts := SyncOptions{Local: localPrefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					_, err := readDataFromArchive(ctx, bucket, remotePrefix)
					assert.Error(t, err)
				},
				"PushIsIdempotent": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					opts := SyncOptions{Local: localPrefix}
					assert.NoError(t, bucket.Push(ctx, opts))
					s3Data, err := readDataFromArchive(ctx, bucket, "")
					require.NoError(t, err)
					assert.Equal(t, data, s3Data)

					assert.NoError(t, bucket.Push(ctx, opts))
					s3Data, err = readDataFromArchive(ctx, bucket, "")
					require.NoError(t, err)
					assert.Equal(t, data, s3Data)
				},
			} {
				t.Run(testName, func(t *testing.T) {
					bucket := impl.constructor(t)
					localPrefix := filepath.Join(tempdir, testutil.NewUUID())
					data := map[string]string{}
					for i := 0; i < 50; i++ {
						file := testutil.NewUUID()
						content := strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
						data[file] = content
						require.NoError(t, writeDataToDisk(localPrefix, file, content))
					}
					testCase(t, bucket, localPrefix, data)
				})
			}
		})
		t.Run("PushToAndPullFromBucket", func(t *testing.T) {
			for testName, testCase := range map[string]func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string){
				"NoPrefix": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					opts := SyncOptions{Local: localPrefix}
					require.NoError(t, bucket.Push(ctx, opts))

					mirror := filepath.Join(tempdir, testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))

					opts = SyncOptions{Local: mirror}
					require.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, data))
				},
				"ShortPrefix": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					remotePrefix := testutil.NewUUID()
					opts := SyncOptions{Local: localPrefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					mirror := filepath.Join(tempdir, testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))

					opts = SyncOptions{Local: mirror, Remote: remotePrefix}
					require.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, data))
				},
				"DryRunBucketDoesNotPush": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					setDryRun(bucket.s3BucketLarge, true)
					remotePrefix := testutil.NewUUID()
					opts := SyncOptions{Local: localPrefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					mirror := filepath.Join(tempdir, testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))

					opts = SyncOptions{Local: mirror, Remote: remotePrefix}
					assert.Error(t, bucket.Pull(ctx, opts))
				},
				"PushWithExcludes": func(t *testing.T, bucket *s3ArchiveBucket, localPrefix string, data map[string]string) {
					dataWithExcluded := map[string]string{}
					for k, v := range data {
						dataWithExcluded[k] = v
					}
					dataWithExcluded["python.py"] = "exclude"
					dataWithExcluded["python2.py"] = "exclude2"
					require.NoError(t, writeDataToDisk(localPrefix, "python.py", dataWithExcluded["python.py"]))
					require.NoError(t, writeDataToDisk(localPrefix, "python2.py", dataWithExcluded["python2.py"]))

					remotePrefix := testutil.NewUUID()
					opts := SyncOptions{Local: localPrefix, Remote: remotePrefix}
					assert.NoError(t, bucket.Push(ctx, opts))

					mirror := filepath.Join(tempdir, testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))
					opts = SyncOptions{Local: mirror, Remote: remotePrefix}
					require.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, dataWithExcluded))

					opts = SyncOptions{Local: localPrefix, Remote: remotePrefix, Exclude: ".*\\.py"}
					require.NoError(t, bucket.Push(ctx, opts))

					mirror = filepath.Join(tempdir, testutil.NewUUID())
					require.NoError(t, os.MkdirAll(mirror, 0777))
					opts = SyncOptions{Local: mirror, Remote: remotePrefix}
					require.NoError(t, bucket.Pull(ctx, opts))

					assert.NoError(t, checkLocalTreeMatchesData(ctx, mirror, data))
				},
			} {
				t.Run(testName, func(t *testing.T) {
					bucket := impl.constructor(t)
					localPrefix := filepath.Join(tempdir, testutil.NewUUID())
					data := map[string]string{}
					for i := 0; i < 50; i++ {
						file := testutil.NewUUID()
						content := strings.Join([]string{testutil.NewUUID(), testutil.NewUUID(), testutil.NewUUID()}, "\n")
						data[file] = content
						require.NoError(t, writeDataToDisk(localPrefix, file, content))
					}
					testCase(t, bucket, localPrefix, data)
				})
			}
		})
	}
}

type mockFileInfo struct {
	name    string
	size    int
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return int64(m.size) }
func (m mockFileInfo) Mode() os.FileMode  { return m.mode }
func (m mockFileInfo) ModTime() time.Time { return m.modTime }
func (m mockFileInfo) IsDir() bool        { return m.isDir }
func (m mockFileInfo) Sys() interface{}   { return nil }

func writeDataToArchive(ctx context.Context, bucket *s3ArchiveBucket, prefix string, data map[string]string) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writer, err := bucket.Writer(wctx, bucket.Join(prefix, syncArchiveName))
	if err != nil {
		return errors.WithStack(err)
	}
	defer writer.Close()
	tarWriter := tar.NewWriter(writer)
	defer tarWriter.Close()

	for name, content := range data {
		info := mockFileInfo{
			name: name,
			size: len(content),
			mode: 0777,
		}
		if err := addToTar(tarWriter, info, bytes.NewBufferString(content), name, name); err != nil {
			return errors.Wrap(err, "adding to tar")
		}
	}

	return errors.WithStack(writer.Close())
}

func readDataFromArchive(ctx context.Context, bucket *s3ArchiveBucket, prefix string) (map[string]string, error) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	data := map[string]string{}

	reader, err := bucket.Reader(rctx, bucket.Join(prefix, syncArchiveName))
	if err != nil {
		return nil, errors.WithStack(err)
	}
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, errors.WithStack(err)
		}
		content, err := ioutil.ReadAll(tarReader)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		data[header.Name] = string(content)
	}
}

func checkLocalTreeMatchesData(ctx context.Context, prefix string, data map[string]string) error {
	files, err := walkLocalTree(ctx, prefix)
	if err != nil {
		return errors.WithStack(err)
	}
	if len(files) != len(data) {
		return errors.Errorf("local file tree has %d items, but expected %d items", len(files), len(data))
	}
	for _, file := range files {
		expectedContent, ok := data[file]
		if !ok {
			return errors.Errorf("file %s should not exist", file)
		}
		content, err := ioutil.ReadFile(filepath.Join(prefix, file))
		if err != nil {
			return errors.Wrapf(err, "could not read file %s", file)
		}
		if string(content) != expectedContent {
			return errors.Errorf("expected content did not match actual content for %s", file)
		}
	}
	return nil
}

func readDataFromFile(ctx context.Context, bucket Bucket, key string) (string, error) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reader, err := bucket.Reader(rctx, key)
	if err != nil {
		return "", errors.WithStack(err)
	}
	out, err := ioutil.ReadAll(reader)
	if err != nil {
		return "", errors.WithStack(err)
	}

	err = reader.Close()
	if err != nil {
		return "", errors.WithStack(err)
	}

	return string(out), nil

}

type brokenWriter struct{}

func (*brokenWriter) Write(_ []byte) (int, error) { return -1, errors.New("always") }
func (*brokenWriter) Read(_ []byte) (int, error)  { return -1, errors.New("always") }

func setDryRun(b Bucket, set bool) {
	switch i := b.(type) {
	case *gridfsBucket:
		i.opts.DryRun = set
	case *localFileSystem:
		i.dryRun = set
	case *s3BucketSmall:
		i.dryRun = set
	case *s3BucketLarge:
		i.dryRun = set
	case *parallelBucketImpl:
		i.dryRun = set
		setDryRun(i.Bucket, set)
	}
}

func setDeleteOnSync(b Bucket, set bool) {
	switch i := b.(type) {
	case *gridfsBucket:
		i.opts.DeleteOnPush = set
		i.opts.DeleteOnPull = set
	case *localFileSystem:
		i.deleteOnPush = set
		i.deleteOnPull = set
	case *s3BucketSmall:
		i.deleteOnPush = set
		i.deleteOnPull = set
	case *s3BucketLarge:
		i.deleteOnPush = set
		i.deleteOnPull = set
	case *parallelBucketImpl:
		i.deleteOnPush = set
		i.deleteOnPull = set
		setDeleteOnSync(i.Bucket, set)
	}
}

func TestPreSign(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	awsKey := os.Getenv("AWS_KEY")
	awsSecret := os.Getenv("AWS_SECRET")
	s3Credentials := CreateAWSStaticCredentials(awsKey, awsSecret, "")
	s3BucketName := "build-test-curator"
	s3Prefix := testutil.NewUUID() + "-"
	s3Object := testutil.NewUUID()
	s3Region := "us-east-1"
	defer func() {
		require.NoError(t, testutil.CleanupS3Bucket(ctx, s3Credentials, s3BucketName, s3Prefix, s3Region))
	}()

	b, err := NewS3Bucket(ctx, S3Options{
		Credentials: s3Credentials,
		Region:      s3Region,
		Name:        s3BucketName,
		Prefix:      s3Prefix,
		MaxRetries:  aws.Int(5),
	})
	require.NoError(t, err)
	data := "hello world"
	require.NoError(t, b.Put(ctx, s3Object, strings.NewReader(data)))

	req := PreSignRequestParams{
		AWSKey:    awsKey,
		AWSSecret: awsSecret,
		Region:    s3Region,
		Bucket:    s3BucketName,
		FileKey:   consistentJoin([]string{s3Prefix, s3Object}),
	}
	url, err := PreSign(ctx, req)
	require.NoError(t, err)
	assert.NotZero(t, url)

	c := utility.GetHTTPClient()
	defer utility.PutHTTPClient(c)

	resp, err := c.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, data, string(body))
}

func TestGetHeadObject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	awsKey := os.Getenv("AWS_KEY")
	awsSecret := os.Getenv("AWS_SECRET")
	s3Credentials := CreateAWSStaticCredentials(awsKey, awsSecret, "")
	s3BucketName := "build-test-curator"
	s3Prefix := testutil.NewUUID() + "-"
	s3Object := testutil.NewUUID()
	s3Region := "us-east-1"
	defer func() {
		require.NoError(t, testutil.CleanupS3Bucket(ctx, s3Credentials, s3BucketName, s3Prefix, s3Region))
	}()

	b, err := NewS3Bucket(ctx, S3Options{
		Credentials: s3Credentials,
		Region:      s3Region,
		Name:        s3BucketName,
		Prefix:      s3Prefix,
		MaxRetries:  aws.Int(5),
	})
	require.NoError(t, err)
	require.NoError(t, b.Put(ctx, s3Object, strings.NewReader("hello world")))

	exists, err := b.Exists(ctx, s3Object)
	require.NoError(t, err)
	assert.True(t, exists)

	t.Run("FailsWithNonexistentObject", func(t *testing.T) {
		req := PreSignRequestParams{
			AWSKey:    awsKey,
			AWSSecret: awsSecret,
			Region:    s3Region,
			Bucket:    s3BucketName,
			FileKey:   consistentJoin([]string{s3Prefix, "DNE"}),
		}
		headObject, err := GetHeadObject(ctx, req)
		assert.Error(t, err)
		assert.Nil(t, headObject)
	})

	t.Run("SucceedsWithExistingObject", func(t *testing.T) {
		req := PreSignRequestParams{
			AWSKey:    awsKey,
			AWSSecret: awsSecret,
			Region:    s3Region,
			Bucket:    s3BucketName,
			FileKey:   consistentJoin([]string{s3Prefix, s3Object}),
		}
		headObject, err := GetHeadObject(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, headObject)
	})
}
