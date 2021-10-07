package cmd

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/treeverse/lakefs/pkg/api"
	"github.com/treeverse/lakefs/pkg/api/helpers"
	"github.com/treeverse/lakefs/pkg/uri"
)

const fsStatTemplate = `Path: {{.Path | yellow }}
Modified Time: {{.Mtime|date}}
Size: {{ .SizeBytes }} bytes
Human Size: {{ .SizeBytes|human_bytes }}
Physical Address: {{ .PhysicalAddress }}
Checksum: {{ .Checksum }}
Content-Type: {{ .ContentType }}
`

const fsRecursiveTemplate = `Files: {{.Count}}
Total Size: {{.Bytes}} bytes
Human Total Size: {{.Bytes|human_bytes}}
`

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

var fsStatCmd = &cobra.Command{
	Use:   "stat <path uri>",
	Short: "View object metadata",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		pathURI := MustParsePathURI("path", args[0])
		client := getClient()
		res, err := client.StatObjectWithResponse(cmd.Context(), pathURI.Repository, pathURI.Ref, &api.StatObjectParams{
			Path: *pathURI.Path,
		})
		DieOnResponseError(res, err)

		stat := res.JSON200
		Write(fsStatTemplate, stat)
	},
}

const fsLsTemplate = `{{ range $val := . -}}
{{ $val.PathType|ljust 12 }}    {{ if eq $val.PathType "object" }}{{ $val.Mtime|date|ljust 29 }}    {{ $val.SizeBytes|human_bytes|ljust 12 }}{{ else }}                                            {{ end }}    {{ $val.Path|yellow }}
{{ end -}}
`

var fsListCmd = &cobra.Command{
	Use:   "ls <path uri>",
	Short: "List entries under a given tree",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		client := getClient()
		pathURI := MustParsePathURI("path", args[0])
		recursive, _ := cmd.Flags().GetBool("recursive")
		prefix := *pathURI.Path

		// prefix we need to trim in ls output (non recursive)
		const delimiter = "/"
		var trimPrefix string
		if idx := strings.LastIndex(prefix, delimiter); idx != -1 {
			trimPrefix = prefix[:idx+1]
		}
		// delimiter used for listing
		var paramsDelimiter api.PaginationDelimiter
		if recursive {
			paramsDelimiter = ""
		} else {
			paramsDelimiter = delimiter
		}
		var from string
		for {
			pfx := api.PaginationPrefix(prefix)
			params := &api.ListObjectsParams{
				Prefix:    &pfx,
				After:     api.PaginationAfterPtr(from),
				Delimiter: &paramsDelimiter,
			}
			resp, err := client.ListObjectsWithResponse(cmd.Context(), pathURI.Repository, pathURI.Ref, params)
			DieOnResponseError(resp, err)

			results := resp.JSON200.Results
			// trim prefix if non recursive
			if !recursive {
				for i := range results {
					trimmed := strings.TrimPrefix(results[i].Path, trimPrefix)
					results[i].Path = trimmed
				}
			}

			Write(fsLsTemplate, results)
			pagination := resp.JSON200.Pagination
			if !pagination.HasMore {
				break
			}
			from = pagination.NextOffset
		}
	},
}

var fsCatCmd = &cobra.Command{
	Use:   "cat <path uri>",
	Short: "Dump content of object to stdout",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		client := getClient()
		pathURI := MustParsePathURI("path", args[0])
		direct, _ := cmd.Flags().GetBool("direct")
		var contents []byte
		if direct {
			_, body, err := helpers.ClientDownload(cmd.Context(), client, pathURI.Repository, pathURI.Ref, *pathURI.Path)
			if err != nil {
				DieErr(err)
			}
			defer body.Close()
			contents, err = io.ReadAll(body)
			if err != nil {
				DieErr(err)
			}
		} else {
			resp, err := client.GetObjectWithResponse(cmd.Context(), pathURI.Repository, pathURI.Ref, &api.GetObjectParams{
				Path: *pathURI.Path,
			})
			DieOnResponseError(resp, err)
			contents = resp.Body
		}
		Fmt("%s\n", string(contents))
	},
}

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}

func upload(ctx context.Context, client api.ClientWithResponsesInterface, sourcePathname string, destURI *uri.URI, contentType string, direct bool) (*api.ObjectStats, error) {
	fp := OpenByPath(sourcePathname)
	defer func() {
		_ = fp.Close()
	}()
	filePath := api.StringValue(destURI.Path)
	if direct {
		return helpers.ClientUpload(ctx, client, destURI.Repository, destURI.Ref, filePath, nil, contentType, fp)
	}
	return uploadObject(ctx, client, destURI.Repository, destURI.Ref, filePath, contentType, fp)
}

func uploadObject(ctx context.Context, client api.ClientWithResponsesInterface, repoID, branchID, filePath, contentType string, fp io.Reader) (*api.ObjectStats, error) {
	pr, pw := io.Pipe()
	mpw := multipart.NewWriter(pw)
	mpContentType := mpw.FormDataContentType()
	go func() {
		defer func() {
			_ = pw.Close()
		}()
		filename := path.Base(filePath)
		const fieldName = "content"
		var err error
		var cw io.Writer
		// when no content-type is specified we let 'CreateFromFile' add the part with the default content type.
		// otherwise, we add a part and set the content-type.
		if contentType != "" {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, escapeQuotes(filename)))
			h.Set("Content-Type", contentType)
			cw, err = mpw.CreatePart(h)
		} else {
			cw, err = mpw.CreateFormFile(fieldName, filename)
		}
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(cw, fp); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = mpw.Close()
	}()

	resp, err := client.UploadObjectWithBodyWithResponse(ctx, repoID, branchID, &api.UploadObjectParams{
		Path: filePath,
	}, mpContentType, pr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusCreated {
		return nil, helpers.ResponseAsError(resp)
	}
	return resp.JSON201, nil
}

var fsUploadCmd = &cobra.Command{
	Use:   "upload <path uri>",
	Short: "Upload a local file to the specified URI",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		client := getClient()
		pathURI := MustParsePathURI("path", args[0])
		source, _ := cmd.Flags().GetString("source")
		recursive, _ := cmd.Flags().GetBool("recursive")
		direct, _ := cmd.Flags().GetBool("direct")
		contentType, _ := cmd.Flags().GetString("content-type")
		if !recursive {
			stat, err := upload(cmd.Context(), client, source, pathURI, contentType, direct)
			if err != nil {
				DieErr(err)
			}
			Write(fsStatTemplate, stat)
			return
		}
		// copy recursively
		var totals struct {
			Bytes int64
			Count int64
		}
		err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return fmt.Errorf("traverse %s: %w", path, err)
			}
			if info.IsDir() {
				return nil
			}
			relPath := strings.TrimPrefix(path, source)
			uri := *pathURI
			p := filepath.Join(*uri.Path, relPath)
			uri.Path = &p
			stat, err := upload(cmd.Context(), client, path, &uri, contentType, direct)
			if err != nil {
				return fmt.Errorf("upload %s: %w", path, err)
			}
			if stat.SizeBytes != nil {
				totals.Bytes += *stat.SizeBytes
			}
			totals.Count++
			return nil
		})
		if err != nil {
			DieErr(err)
		}
		Write(fsRecursiveTemplate, totals)
	},
}

var fsStageCmd = &cobra.Command{
	Use:    "stage <path uri>",
	Short:  "Stage a reference to an existing object, to be managed in lakeFS",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		client := getClient()
		pathURI := MustParsePathURI("path", args[0])
		flags := cmd.Flags()
		size, _ := flags.GetInt64("size")
		mtimeSeconds, _ := flags.GetInt64("mtime")
		location, _ := flags.GetString("location")
		checksum, _ := flags.GetString("checksum")
		contentType, _ := flags.GetString("content-type")
		meta, metaErr := getKV(cmd, "meta")

		var mtime *int64
		if mtimeSeconds != 0 {
			mtime = &mtimeSeconds
		}

		obj := api.ObjectStageCreation{
			Checksum:        checksum,
			Mtime:           mtime,
			PhysicalAddress: location,
			SizeBytes:       size,
			ContentType:     &contentType,
		}
		if metaErr == nil {
			metadata := api.ObjectUserMetadata{
				AdditionalProperties: meta,
			}
			obj.Metadata = &metadata
		}

		resp, err := client.StageObjectWithResponse(cmd.Context(), pathURI.Repository, pathURI.Ref, &api.StageObjectParams{
			Path: *pathURI.Path,
		}, api.StageObjectJSONRequestBody(obj))
		DieOnResponseError(resp, err)

		Write(fsStatTemplate, resp.JSON201)
	},
}

func initDeleteWorkerPool(ctx context.Context, client api.ClientWithResponsesInterface, paths chan string, numWorkers int, wg *sync.WaitGroup) {
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go deleteObjectWorker(ctx, client, paths, wg)
	}
}

func deleteObjectWorker(ctx context.Context, client api.ClientWithResponsesInterface, paths <-chan string, wg *sync.WaitGroup) {
	for path := range paths {
		pathURI := MustParsePathURI("path", path)
		deleteObject(ctx, client, pathURI)
	}
	defer wg.Done()
}

func deleteObject(ctx context.Context, client api.ClientWithResponsesInterface, pathURI *uri.URI) {
	resp, err := client.DeleteObjectWithResponse(ctx, pathURI.Repository, pathURI.Ref, &api.DeleteObjectParams{
		Path: *pathURI.Path,
	})
	DieOnResponseError(resp, err)
}

var fsRmCmd = &cobra.Command{
	Use:   "rm <path uri>",
	Short: "Delete object",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		recursive, _ := cmd.Flags().GetBool("recursive")
		pathURI := MustParsePathURI("path", args[0])
		client := getClient()
		if !recursive {
			// Delete single object in the main thread
			deleteObject(cmd.Context(), client, pathURI)
			return
		}
		// Recursive delete of (possibly) many objects.
		const numWorkers = 50
		var wg sync.WaitGroup
		paths := make(chan string)
		initDeleteWorkerPool(cmd.Context(), client, paths, numWorkers, &wg)

		prefix := *pathURI.Path
		const delimiter = "/"
		var trimPrefix string
		if idx := strings.LastIndex(prefix, delimiter); idx != -1 {
			trimPrefix = prefix[:idx+1]
		}
		var paramsDelimiter api.PaginationDelimiter = ""
		var from string
		for {
			pfx := api.PaginationPrefix(prefix)
			params := &api.ListObjectsParams{
				Prefix:    &pfx,
				After:     api.PaginationAfterPtr(from),
				Delimiter: &paramsDelimiter,
			}
			resp, err := client.ListObjectsWithResponse(cmd.Context(), pathURI.Repository, pathURI.Ref, params)
			DieOnResponseError(resp, err)

			results := resp.JSON200.Results
			for _, result := range results {
				currPath := pathURI.String() + strings.TrimPrefix(result.Path, trimPrefix)
				paths <- currPath
			}

			pagination := resp.JSON200.Pagination
			if !pagination.HasMore {
				break
			}
			from = pagination.NextOffset
		}
		close(paths)
		wg.Wait()
	},
}

// fsCmd represents the fs command
var fsCmd = &cobra.Command{
	Use:   "fs",
	Short: "View and manipulate objects",
}

//nolint:gochecknoinits
func init() {
	rootCmd.AddCommand(fsCmd)
	fsCmd.AddCommand(fsStatCmd)
	fsCmd.AddCommand(fsListCmd)
	fsCmd.AddCommand(fsCatCmd)
	fsCmd.AddCommand(fsUploadCmd)
	fsCmd.AddCommand(fsStageCmd)
	fsCmd.AddCommand(fsRmCmd)

	fsCatCmd.Flags().BoolP("direct", "d", false, "read directly from backing store (faster but requires more credentials)")

	fsUploadCmd.Flags().StringP("source", "s", "", "local file to upload, or \"-\" for stdin")
	fsUploadCmd.Flags().BoolP("recursive", "r", false, "recursively copy all files under local source")
	fsUploadCmd.Flags().BoolP("direct", "d", false, "write directly to backing store (faster but requires more credentials)")
	_ = fsUploadCmd.MarkFlagRequired("source")
	fsUploadCmd.Flags().StringP("content-type", "", "", "MIME type of contents")

	fsStageCmd.Flags().String("location", "", "fully qualified storage location (i.e. \"s3://bucket/path/to/object\")")
	fsStageCmd.Flags().Int64("size", 0, "Object size in bytes")
	fsStageCmd.Flags().String("checksum", "", "Object MD5 checksum as a hexadecimal string")
	fsStageCmd.Flags().Int64("mtime", 0, "Object modified time (Unix Epoch in seconds). Defaults to current time")
	fsStageCmd.Flags().String("content-type", "", "MIME type of contents")
	fsStageCmd.Flags().StringSlice("meta", []string{}, "key value pairs in the form of key=value")
	_ = fsStageCmd.MarkFlagRequired("location")
	_ = fsStageCmd.MarkFlagRequired("size")
	_ = fsStageCmd.MarkFlagRequired("checksum")

	fsListCmd.Flags().Bool("recursive", false, "list all objects under the specified prefix")

	fsRmCmd.Flags().Bool("recursive", false, "recursively delete all objects under the specified path")
}
