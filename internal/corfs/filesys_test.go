package corfs

import (
	"testing"

	"github.com/ISE-SMILE/corral/api"
	"github.com/stretchr/testify/assert"
)

func TestInitFilesystem(t *testing.T) {
	fs, _ := InitFilesystem(api.S3)
	assert.NotNil(t, fs)
	assert.IsType(t, &S3FileSystem{}, fs)

	fs, _ = InitFilesystem(api.Local)
	assert.NotNil(t, fs)
	assert.IsType(t, &LocalFileSystem{}, fs)
}

func TestInferFilesystem(t *testing.T) {
	fs := InferFilesystem("s3://foo/bar.txt")
	assert.NotNil(t, fs)
	assert.IsType(t, &S3FileSystem{}, fs)

	fs = InferFilesystem("./bar.txt")
	assert.NotNil(t, fs)
	assert.IsType(t, &LocalFileSystem{}, fs)
}
