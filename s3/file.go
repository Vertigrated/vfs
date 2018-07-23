package s3

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"

	"github.com/c2fo/vfs"
	"github.com/c2fo/vfs/mocks"
)

//File implements vfs.File interface for S3 fs.
type File struct {
	fileSystem  *FileSystem
	bucket      string
	key         string
	tempFile    *os.File
	writeBuffer *bytes.Buffer
}

// newFile initializer returns a pointer to File.
func newFile(fs *FileSystem, bucket, key string) (*File, error) {
	if fs == nil {
		return nil, errors.New("non-nil s3.fileSystem pointer is required")
	}
	if bucket == "" || key == "" {
		return nil, errors.New("non-empty strings for bucket and key are required")
	}
	key = vfs.CleanPrefix(key)
	return &File{
		fileSystem: fs,
		bucket:     bucket,
		key:        key,
	}, nil
}

// Info Functions

// LastModified returns the LastModified property of a HEAD request to the s3 object.
func (f *File) LastModified() (*time.Time, error) {
	head, err := f.getHeadObject()
	if err != nil {
		return nil, err
	}
	return head.LastModified, nil
}

// Name returns the name portion of the file's key property. IE: "file.txt" of "s3://some/path/to/file.txt
func (f *File) Name() string {
	return path.Base(f.key)
}

// Path return the directory portion of the file's key. IE: "path/to" of "s3://some/path/to/file.txt
func (f *File) Path() string {
	return "/" + f.key
}

// Exists returns a boolean of whether or not the object exists on s3, based on a call for
// the object's HEAD through the s3 API.
func (f *File) Exists() (bool, error) {
	_, err := f.getHeadObject()
	code := ""
	if err != nil {
		code = err.(awserr.Error).Code()
	}
	if err != nil && (code == s3.ErrCodeNoSuchKey || code == "NotFound") {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

// Size returns the ContentLength value from an s3 HEAD request on the file's object.
func (f *File) Size() (uint64, error) {
	head, err := f.getHeadObject()
	if err != nil {
		return 0, err
	}
	return uint64(*head.ContentLength), nil
}

// Location returns a vfs.Location at the location of the object. IE: if file is at
// s3://bucket/here/is/the/file.txt the location points to s3://bucket/here/is/the/
func (f *File) Location() vfs.Location {
	return vfs.Location(&Location{
		fileSystem: f.fileSystem,
		prefix:     path.Dir(f.key),
		bucket:     f.bucket,
	})
}

// Move/Copy Operations

// CopyToFile puts the contents of File into the targetFile passed. Uses the S3 CopyObject
// method if the target file is also on S3, otherwise uses io.Copy.
func (f *File) CopyToFile(targetFile vfs.File) error {
	if tf, ok := targetFile.(*File); ok {
		return f.copyWithinS3ToFile(tf)
	}

	if err := vfs.TouchCopy(targetFile, f); err != nil {
		return err
	}
	//Close target to flush and ensure that cursor isn't at the end of the file when the caller reopens for read
	if cerr := targetFile.Close(); cerr != nil {
		return cerr
	}
	//Close file (f) reader
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	return nil
}

// MoveToFile puts the contents of File into the targetFile passed using File.CopyToFile.
// If the copy succeeds, the source file is deleted. Any errors from the copy or delete are
// returned.
func (f *File) MoveToFile(targetFile vfs.File) error {
	if err := f.CopyToFile(targetFile); err != nil {
		return err
	}

	return f.Delete()
}

// MoveToLocation works by first calling File.CopyToLocation(vfs.Location) then, if that
// succeeds, it deletes the original file, returning the new file. If the copy process fails
// the error is returned, and the Delete isn't called. If the call to Delete fails, the error
// and the file generated by the copy are both returned.
func (f *File) MoveToLocation(location vfs.Location) (vfs.File, error) {
	newFile, err := f.CopyToLocation(location)
	if err != nil {
		return nil, err
	}
	delErr := f.Delete()
	return newFile, delErr
}

// CopyToLocation creates a copy of *File, using the file's current name as the new file's
// name at the given location. If the given location is also s3, the AWS API for copying
// files will be utilized, otherwise, standard io.Copy will be done to the new file.
func (f *File) CopyToLocation(location vfs.Location) (vfs.File, error) {
	// This is a copy to s3, from s3, we should attempt to utilize the AWS S3 API for this.
	if location.FileSystem().Scheme() == Scheme {
		return f.copyWithinS3ToLocation(location)
	}

	newFile, err := location.FileSystem().NewFile(location.Volume(), path.Join(location.Path(), f.Name()))
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(newFile, f); err != nil {
		return nil, err
	}
	//Close target file to flush and ensure that cursor isn't at the end of the file when the caller reopens for read
	if cerr := newFile.Close(); cerr != nil {
		return nil, cerr
	}
	//Close file (f) reader
	if cerr := f.Close(); cerr != nil {
		return nil, cerr
	}
	return newFile, nil
}

// CRUD Operations

// Delete clears any local temp file, or write buffer from read/writes to the file, then makes
// a DeleteObject call to s3 for the file. Returns any error returned by the API.
func (f *File) Delete() error {
	f.writeBuffer = nil
	if err := f.Close(); err != nil {
		return err
	}

	_, err := f.fileSystem.Client.DeleteObject(&s3.DeleteObjectInput{
		Key:    &f.key,
		Bucket: &f.bucket,
	})
	return err
}

// Close cleans up underlying mechanisms for reading from and writing to the file. Closes and removes the
// local temp file, and triggers a write to s3 of anything in the f.writeBuffer if it has been created.
func (f *File) Close() (rerr error) {
	//setup multi error return using named error
	errs := vfs.NewMutliErr()
	defer func() { rerr = errs.OrNil() }()

	if f.tempFile != nil {
		defer errs.DeferFunc(f.tempFile.Close)

		err := os.Remove(f.tempFile.Name())
		if err != nil && !os.IsNotExist(err) {
			return errs.Append(err)
		}

		f.tempFile = nil
	}

	if f.writeBuffer != nil {
		uploader := s3manager.NewUploaderWithClient(f.fileSystem.Client)
		uploadInput := f.uploadInput()
		uploadInput.Body = f.writeBuffer
		_, err := uploader.Upload(uploadInput)
		if err != nil {
			return errs.Append(err)
		}
	}

	f.writeBuffer = nil

	if err := waitUntilFileExists(f, 5); err != nil {
		return err
	}
	return nil
}

// Read implements the standard for io.Reader. For this to work with an s3 file, a temporary local copy of
// the file is created, and reads work on that. This file is closed and removed upon calling f.Close()
func (f *File) Read(p []byte) (n int, err error) {
	if err := f.checkTempFile(); err != nil {
		return 0, err
	}
	return f.tempFile.Read(p)
}

// Seek implements the standard for io.Seeker. A temporary local copy of the s3 file is created (the same
// one used for Reads) which Seek() acts on. This file is closed and removed upon calling f.Close()
func (f *File) Seek(offset int64, whence int) (int64, error) {
	if err := f.checkTempFile(); err != nil {
		return 0, err
	}
	return f.tempFile.Seek(offset, whence)
}

// Write implements the standard for io.Writer. A buffer is added to with each subsequent
// write. When f.Close() is called, the contents of the buffer are used to initiate the
// PutObject to s3. The underlying implementation uses s3manager which will determine whether
// it is appropriate to call PutObject, or initiate a multi-part upload.
func (f *File) Write(data []byte) (res int, err error) {
	if f.writeBuffer == nil {
		//note, initializing with 'data' and returning len(data), nil
		//causes issues with some Write usages, notably csv.Writer
		//so we simply intialize with no bytes and call the buffer Write after
		//
		//f.writeBuffer = bytes.NewBuffer(data)
		//return len(data), nil
		//
		//so now we do:

		f.writeBuffer = bytes.NewBuffer([]byte{})
	}
	return f.writeBuffer.Write(data)
}

// URI returns the File's URI as a string.
func (f *File) URI() string {
	return vfs.GetFileURI(f)
}

// String implement fmt.Stringer, returning the file's URI as the default string.
func (f *File) String() string {
	return f.URI()
}

/*
	Private helper functions
*/
func (f *File) getHeadObject() (*s3.HeadObjectOutput, error) {
	headObjectInput := new(s3.HeadObjectInput).SetKey(f.key).SetBucket(f.bucket)
	return f.fileSystem.Client.HeadObject(headObjectInput)
}

func (f *File) copyWithinS3ToFile(targetFile *File) error {
	copyInput := new(s3.CopyObjectInput).SetKey(targetFile.key).SetBucket(targetFile.bucket).SetCopySource(path.Join(f.bucket, f.key))
	_, err := f.fileSystem.Client.CopyObject(copyInput)

	return err
}

func (f *File) copyWithinS3ToLocation(location vfs.Location) (vfs.File, error) {
	copyInput := new(s3.CopyObjectInput).SetKey(path.Join(location.Path(), f.Name())).SetBucket(location.Volume()).SetCopySource(path.Join(f.bucket, f.key))
	_, err := f.fileSystem.Client.CopyObject(copyInput)
	if err != nil {
		return nil, err
	}

	return location.FileSystem().NewFile(location.Volume(), path.Join(location.Path(), f.Name()))
}

func (f *File) checkTempFile() error {
	if f.tempFile == nil {
		localTempFile, err := f.copyToLocalTempReader()
		if err != nil {
			return err
		}
		f.tempFile = localTempFile
	}

	return nil
}

func (f *File) copyToLocalTempReader() (*os.File, error) {
	tmpFile, err := ioutil.TempFile("", fmt.Sprintf("%s.%d", f.Name(), time.Now().UnixNano()))
	if err != nil {
		return nil, err
	}

	outputReader, err := f.getObject()
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(tmpFile, outputReader); err != nil {
		return nil, err
	}

	// Return cursor to the beginning of the new temp file
	if _, err := tmpFile.Seek(0, 0); err != nil {
		return nil, err
	}

	//initialize temp ReadCloser
	return tmpFile, nil
}

func (f *File) putObjectInput() *s3.PutObjectInput {
	return new(s3.PutObjectInput).SetBucket(f.bucket).SetKey(f.key)
}

func (f *File) putObject(reader io.ReadSeeker) error {
	_, err := f.fileSystem.Client.PutObject(f.putObjectInput().SetBody(reader))

	return err
}

//TODO: need to provide an implementation-agnostic container for providing config options such as SSE
func (f *File) uploadInput() *s3manager.UploadInput {
	sseType := "AES256"
	return &s3manager.UploadInput{
		Bucket:               &f.bucket,
		Key:                  &f.key,
		ServerSideEncryption: &sseType,
	}
}

func (f *File) getObjectInput() *s3.GetObjectInput {
	return new(s3.GetObjectInput).SetBucket(f.bucket).SetKey(f.key)
}

func (f *File) getObject() (io.ReadCloser, error) {
	getOutput, err := f.fileSystem.Client.GetObject(f.getObjectInput())
	if err != nil {
		return nil, err
	}

	return getOutput.Body, nil
}

//WaitUntilFileExists attempts to ensure that a recently written file is available before moving on.  This is helpful for
// attempting to overcome race conditions withe S3's "eventual consistency".
// WaitUntilFileExists accepts vfs.File and an int representing the number of times to retry(once a second).
// error is returned if the file is still not available after the specified retries.
// nil is returned once the file is available.
func waitUntilFileExists(file vfs.File, retries int) error {
	// Ignore in-memory VFS files
	if _, ok := file.(*mocks.ReadWriteFile); ok {
		return nil
	}

	// Return as if file was found when retries is set to -1. Useful mainly for testing.
	if retries == -1 {
		return nil
	}
	var retryCount = 0
	for {
		if retryCount == retries {
			return errors.New(fmt.Sprintf("Failed to find file %s after %d", file, retries))
		}

		//check for existing file
		found, err := file.Exists()
		if err != nil {
			return errors.New("unable to check for file on S3")
		}

		if found {
			break
		}

		retryCount++
		time.Sleep(time.Second * 1)
	}

	return nil
}
