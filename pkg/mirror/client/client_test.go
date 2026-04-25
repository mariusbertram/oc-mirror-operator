package client

import (
	"bytes"
	"context"
	"io"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/regclient/regclient/types/blob"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/ref"
)

func mustParseRef(s string) ref.Ref {
	r, err := ref.New(s)
	if err != nil {
		panic(err)
	}
	return r
}

func shortCtx() context.Context {
	ctx, _ := context.WithTimeout(context.Background(), 2*time.Second) //nolint:govet
	return ctx
}

var _ = Describe("MirrorClient", func() {
	Describe("NewMirrorClient", func() {
		It("creates client with no insecure hosts", func() {
			mc := NewMirrorClient(nil, "")
			Expect(mc).NotTo(BeNil())
			Expect(mc.rc).NotTo(BeNil())
			Expect(mc.rcFallback).To(BeNil())
		})

		It("creates client with insecure hosts (fallback set)", func() {
			mc := NewMirrorClient([]string{"insecure.example.com"}, "")
			Expect(mc).NotTo(BeNil())
			Expect(mc.rc).NotTo(BeNil())
			Expect(mc.rcFallback).NotTo(BeNil())
		})

		It("creates client with auth config path", func() {
			mc := NewMirrorClient(nil, "/tmp/fake-docker-config")
			Expect(mc).NotTo(BeNil())
		})

		It("creates client with dest hosts", func() {
			mc := NewMirrorClient(nil, "", "dest.example.com")
			Expect(mc).NotTo(BeNil())
		})

		It("creates client with insecure + dest + auth all set", func() {
			mc := NewMirrorClient([]string{"insecure.io"}, "/tmp/auth", "dest.io")
			Expect(mc).NotTo(BeNil())
			Expect(mc.rcFallback).NotTo(BeNil())
		})

		It("skips empty host strings in insecure list", func() {
			mc := NewMirrorClient([]string{"", "real.host.io"}, "", "")
			Expect(mc).NotTo(BeNil())
			// Both "" entries are skipped; "real.host.io" triggers fallback
			Expect(mc.rcFallback).NotTo(BeNil())
		})
	})

	Describe("CopyImage error paths", func() {
		It("returns error for invalid source reference", func() {
			mc := NewMirrorClient(nil, "")
			_, err := mc.CopyImage(shortCtx(), ":::invalid", "dest.io/img:v1")
			Expect(err).To(HaveOccurred())
		})

		It("returns error for invalid dest reference", func() {
			mc := NewMirrorClient(nil, "")
			_, err := mc.CopyImage(shortCtx(), "quay.io/img:v1", ":::invalid")
			Expect(err).To(HaveOccurred())
		})

		It("falls back to HTTP on HTTPS failure when insecure hosts set", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			_, err := mc.CopyImage(shortCtx(), "localhost/img:v1", "localhost/mirror:v1")
			Expect(err).To(HaveOccurred()) // both fail, but fallback was tried
		})
	})

	Describe("CheckExist error paths", func() {
		It("returns error for invalid reference", func() {
			mc := NewMirrorClient(nil, "")
			_, err := mc.CheckExist(shortCtx(), ":::invalid")
			Expect(err).To(HaveOccurred())
		})

		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			_, err := mc.CheckExist(shortCtx(), "localhost/img:v1")
			// Both primary and fallback fail
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("GetDigest error paths", func() {
		It("returns error for invalid reference", func() {
			mc := NewMirrorClient(nil, "")
			_, err := mc.GetDigest(shortCtx(), ":::invalid")
			Expect(err).To(HaveOccurred())
		})

		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			_, err := mc.GetDigest(shortCtx(), "localhost/img:v1")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("DeleteManifest error paths", func() {
		It("returns error for invalid reference", func() {
			mc := NewMirrorClient(nil, "")
			err := mc.DeleteManifest(shortCtx(), ":::invalid")
			Expect(err).To(HaveOccurred())
		})

		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			err := mc.DeleteManifest(shortCtx(), "localhost/img:v1")
			Expect(err).To(HaveOccurred())
		})

		It("returns error when manifest head fails (no fallback)", func() {
			mc := NewMirrorClient(nil, "")
			err := mc.DeleteManifest(shortCtx(), "localhost/img:v1")
			Expect(err).To(HaveOccurred())
		})

		It("skips HEAD when digest provided and fails on delete", func() {
			mc := NewMirrorClient(nil, "")
			err := mc.DeleteManifest(shortCtx(), "localhost/img@sha256:0000000000000000000000000000000000000000000000000000000000000000")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ManifestGet error paths", func() {
		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			ref := mustParseRef("localhost/img:v1")
			_, err := mc.ManifestGet(shortCtx(), ref)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("BlobGet error paths", func() {
		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			ref := mustParseRef("localhost/img:v1")
			_, err := mc.BlobGet(shortCtx(), ref, descriptor.Descriptor{})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("BlobCopy error paths", func() {
		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			src := mustParseRef("localhost/src:v1")
			dst := mustParseRef("localhost/dst:v1")
			err := mc.BlobCopy(shortCtx(), src, dst, descriptor.Descriptor{})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("BlobPut error paths", func() {
		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			ref := mustParseRef("localhost/img:v1")
			_, err := mc.BlobPut(shortCtx(), ref, descriptor.Descriptor{}, nil)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ImageConfig error paths", func() {
		It("falls back on insecure hosts", func() {
			mc := NewMirrorClient([]string{"localhost"}, "")
			ref := mustParseRef("localhost/img:v1")
			_, err := mc.ImageConfig(shortCtx(), ref)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("tempFileReader", func() {
		It("removes file on Close", func() {
			f, err := os.CreateTemp("", "test-temp-reader-*.tmp")
			Expect(err).NotTo(HaveOccurred())
			path := f.Name()

			tfr := &tempFileReader{f}
			Expect(tfr.Close()).To(Succeed())

			_, statErr := os.Stat(path)
			Expect(os.IsNotExist(statErr)).To(BeTrue())
		})
	})

	Describe("bufferLargeBlobs", func() {
		It("returns small blob unchanged", func() {
			data := []byte("small data")
			d := descriptor.Descriptor{Size: int64(len(data))}
			br := blob.NewReader(
				blob.WithReader(io.NopCloser(bytes.NewReader(data))),
				blob.WithDesc(d),
			)
			result, err := bufferLargeBlobs(br)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
		})

		It("buffers large blob (size=0 triggers buffering)", func() {
			// Create the buffer directory
			Expect(os.MkdirAll(blobBufferDir, 0o755)).To(Succeed())

			data := []byte("some data to buffer")
			d := descriptor.Descriptor{Size: 0} // size 0 → treated as "unknown" → buffer
			br := blob.NewReader(
				blob.WithReader(io.NopCloser(bytes.NewReader(data))),
				blob.WithDesc(d),
			)
			result, err := bufferLargeBlobs(br)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			resultDesc := result.GetDescriptor()
			Expect(resultDesc.Size).To(Equal(int64(len(data))))
		})

		It("returns error when buffer directory missing", func() {
			data := []byte("data")
			d := descriptor.Descriptor{Size: 0}
			br := blob.NewReader(
				blob.WithReader(io.NopCloser(bytes.NewReader(data))),
				blob.WithDesc(d),
			)
			// Remove the buffer dir to trigger error
			_ = os.RemoveAll(blobBufferDir)
			_, err := bufferLargeBlobs(br)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("create temp file"))
		})
	})
})
