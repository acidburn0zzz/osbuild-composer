//go:generate go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen --package=v2 --generate types,spec,server -o openapi.v2.gen.go openapi.v2.yml
package v2

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/sirupsen/logrus"

	"github.com/osbuild/osbuild-composer/internal/blueprint"
	"github.com/osbuild/osbuild-composer/internal/common"
	"github.com/osbuild/osbuild-composer/internal/distro"
	"github.com/osbuild/osbuild-composer/internal/distroregistry"
	"github.com/osbuild/osbuild-composer/internal/jobqueue"
	osbuild "github.com/osbuild/osbuild-composer/internal/osbuild2"
	"github.com/osbuild/osbuild-composer/internal/ostree"
	"github.com/osbuild/osbuild-composer/internal/prometheus"
	"github.com/osbuild/osbuild-composer/internal/rpmmd"
	"github.com/osbuild/osbuild-composer/internal/target"
	"github.com/osbuild/osbuild-composer/internal/worker"
	"github.com/osbuild/osbuild-composer/internal/worker/clienterrors"
)

// Server represents the state of the cloud Server
type Server struct {
	workers     *worker.Server
	rpmMetadata rpmmd.RPMMD
	distros     *distroregistry.Registry
	awsBucket   string
}

type apiHandlers struct {
	server *Server
}

type binder struct{}

func NewServer(workers *worker.Server, rpmMetadata rpmmd.RPMMD, distros *distroregistry.Registry, bucket string) *Server {
	server := &Server{
		workers:     workers,
		rpmMetadata: rpmMetadata,
		distros:     distros,
		awsBucket:   bucket,
	}
	return server
}

func (server *Server) Handler(path string) http.Handler {
	e := echo.New()
	e.Binder = binder{}
	e.HTTPErrorHandler = server.HTTPErrorHandler
	e.Pre(common.OperationIDMiddleware)
	e.Use(middleware.Recover())
	e.Logger = common.Logger()

	handler := apiHandlers{
		server: server,
	}
	RegisterHandlers(e.Group(path, prometheus.MetricsMiddleware), &handler)

	return e
}

func (b binder) Bind(i interface{}, ctx echo.Context) error {
	contentType := ctx.Request().Header["Content-Type"]
	if len(contentType) != 1 || contentType[0] != "application/json" {
		return HTTPError(ErrorUnsupportedMediaType)
	}

	err := json.NewDecoder(ctx.Request().Body).Decode(i)
	if err != nil {
		return HTTPErrorWithInternal(ErrorBodyDecodingError, err)
	}
	return nil
}

func (h *apiHandlers) GetOpenapi(ctx echo.Context) error {
	spec, err := GetSwagger()
	if err != nil {
		return HTTPError(ErrorFailedToLoadOpenAPISpec)
	}
	return ctx.JSON(http.StatusOK, spec)
}

func (h *apiHandlers) GetErrorList(ctx echo.Context, params GetErrorListParams) error {
	page := 0
	var err error
	if params.Page != nil {
		page, err = strconv.Atoi(string(*params.Page))
		if err != nil {
			return HTTPError(ErrorInvalidPageParam)
		}
	}

	size := 100
	if params.Size != nil {
		size, err = strconv.Atoi(string(*params.Size))
		if err != nil {
			return HTTPError(ErrorInvalidSizeParam)
		}
	}

	return ctx.JSON(http.StatusOK, APIErrorList(page, size, ctx))
}

func (h *apiHandlers) GetError(ctx echo.Context, id string) error {
	errorId, err := strconv.Atoi(id)
	if err != nil {
		return HTTPError(ErrorInvalidErrorId)
	}

	apiError := APIError(ServiceErrorCode(errorId), nil, ctx)
	// If the service error wasn't found, it's a 404 in this instance
	if apiError.Id == fmt.Sprintf("%d", ErrorServiceErrorNotFound) {
		return HTTPError(ErrorErrorNotFound)
	}
	return ctx.JSON(http.StatusOK, apiError)
}

// splitExtension returns the extension of the given file. If there's
// a multipart extension (e.g. file.tar.gz), it returns all parts (e.g.
// .tar.gz). If there's no extension in the input, it returns an empty
// string. If the filename starts with dot, the part before the second dot
// is not considered as an extension.
func splitExtension(filename string) string {
	filenameParts := strings.Split(filename, ".")

	if len(filenameParts) > 0 && filenameParts[0] == "" {
		filenameParts = filenameParts[1:]
	}

	if len(filenameParts) <= 1 {
		return ""
	}

	return "." + strings.Join(filenameParts[1:], ".")
}

type imageRequest struct {
	imageType               distro.ImageType
	arch                    distro.Arch
	repositories            []rpmmd.RepoConfig
	packageSetsRepositories map[string][]rpmmd.RepoConfig
	imageOptions            distro.ImageOptions
	target                  *target.Target
}

func (h *apiHandlers) PostCompose(ctx echo.Context) error {
	var request ComposeRequest
	err := ctx.Bind(&request)
	if err != nil {
		return err
	}

	distribution := h.server.distros.GetDistro(request.Distribution)
	if distribution == nil {
		return HTTPError(ErrorUnsupportedDistribution)
	}

	var bp = blueprint.Blueprint{}
	err = bp.Initialize()
	if err != nil {
		return HTTPErrorWithInternal(ErrorFailedToInitializeBlueprint, err)
	}

	// Set the blueprint customisation to take care of the user
	if request.Customizations != nil && request.Customizations.Users != nil {
		var userCustomizations []blueprint.UserCustomization
		for _, user := range *request.Customizations.Users {
			var groups []string
			if user.Groups != nil {
				groups = *user.Groups
			} else {
				groups = nil
			}
			userCustomizations = append(userCustomizations,
				blueprint.UserCustomization{
					Name:   user.Name,
					Key:    user.Key,
					Groups: groups,
				},
			)
		}
		bp.Customizations = &blueprint.Customizations{
			User: userCustomizations,
		}
	}

	if request.Customizations != nil && request.Customizations.Packages != nil {
		for _, p := range *request.Customizations.Packages {
			bp.Packages = append(bp.Packages, blueprint.Package{
				Name: p,
			})
		}
	}

	var payloadRepositories []Repository
	if request.Customizations != nil && request.Customizations.PayloadRepositories != nil {
		payloadRepositories = *request.Customizations.PayloadRepositories
	}

	// use the same seed for all images so we get the same IDs
	bigSeed, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return HTTPError(ErrorFailedToGenerateManifestSeed)
	}
	manifestSeed := bigSeed.Int64()

	// For backwards compatibility, we support both a single image request
	// as well as an array of requests in the API. Exactly one must be
	// specified.
	if request.ImageRequest != nil {
		if request.ImageRequests != nil {
			// we should really be using oneOf in the spec
			return HTTPError(ErrorInvalidNumberOfImageBuilds)
		}
		request.ImageRequests = &[]ImageRequest{*request.ImageRequest}
	}
	if request.ImageRequests == nil {
		return HTTPError(ErrorInvalidNumberOfImageBuilds)
	}
	var irs []imageRequest
	for _, ir := range *request.ImageRequests {
		arch, err := distribution.GetArch(ir.Architecture)
		if err != nil {
			return HTTPError(ErrorUnsupportedArchitecture)
		}
		imageType, err := arch.GetImageType(imageTypeFromApiImageType(ir.ImageType))
		if err != nil {
			return HTTPError(ErrorUnsupportedImageType)
		}
		repositories := make([]rpmmd.RepoConfig, len(ir.Repositories))
		for j, repo := range ir.Repositories {
			repositories[j].RHSM = repo.Rhsm

			if repo.Baseurl != nil {
				repositories[j].BaseURL = *repo.Baseurl
			} else if repo.Mirrorlist != nil {
				repositories[j].MirrorList = *repo.Mirrorlist
			} else if repo.Metalink != nil {
				repositories[j].Metalink = *repo.Metalink
			} else {
				return HTTPError(ErrorInvalidRepository)
			}
		}

		payloadPackageSets := imageType.PayloadPackageSets()
		packageSetsRepositories := make(map[string][]rpmmd.RepoConfig, len(payloadPackageSets))

		for _, packageSetKey := range payloadPackageSets {
			packageSetsRepositories[packageSetKey] = make([]rpmmd.RepoConfig, len(payloadRepositories))
			for j, repo := range payloadRepositories {
				if repo.Baseurl != nil {
					packageSetsRepositories[packageSetKey][j].BaseURL = *repo.Baseurl
				} else {
					return HTTPError(ErrorNoBaseURLInPayloadRepository)
				}
				if repo.GpgKey != nil {
					packageSetsRepositories[packageSetKey][j].GPGKey = *repo.GpgKey
				}
				if repo.CheckGpg != nil {
					packageSetsRepositories[packageSetKey][j].CheckGPG = *repo.CheckGpg
				}
				if repo.IgnoreSsl != nil {
					packageSetsRepositories[packageSetKey][j].IgnoreSSL = *repo.IgnoreSsl
				}
			}
		}

		imageOptions := distro.ImageOptions{Size: imageType.Size(0)}
		if request.Customizations != nil && request.Customizations.Subscription != nil {
			imageOptions.Subscription = &distro.SubscriptionImageOptions{
				Organization:  request.Customizations.Subscription.Organization,
				ActivationKey: request.Customizations.Subscription.ActivationKey,
				ServerUrl:     request.Customizations.Subscription.ServerUrl,
				BaseUrl:       request.Customizations.Subscription.BaseUrl,
				Insights:      request.Customizations.Subscription.Insights,
			}
		}

		// set default ostree ref, if one not provided
		ostreeOptions := ir.Ostree
		if ostreeOptions == nil || ostreeOptions.Ref == nil {
			imageOptions.OSTree = distro.OSTreeImageOptions{Ref: imageType.OSTreeRef()}
		} else if !ostree.VerifyRef(*ostreeOptions.Ref) {
			return HTTPError(ErrorInvalidOSTreeRef)
		} else {
			imageOptions.OSTree = distro.OSTreeImageOptions{Ref: *ostreeOptions.Ref}
		}

		var parent string
		if ostreeOptions != nil && ostreeOptions.Url != nil {
			imageOptions.OSTree.URL = *ostreeOptions.Url
			parent, err = ostree.ResolveRef(imageOptions.OSTree.URL, imageOptions.OSTree.Ref)
			if err != nil {
				return HTTPErrorWithInternal(ErrorInvalidOSTreeRepo, err)
			}
			imageOptions.OSTree.Parent = parent
		}

		var irTarget *target.Target
		if ir.UploadOptions == nil {
			// nowhere to put the image, this is a user error
			if request.Koji == nil {
				return HTTPError(ErrorJSONUnMarshallingError)
			}
		} else {
			// TODO: support uploads also for koji
			if request.Koji != nil {
				return HTTPError(ErrorJSONUnMarshallingError)
			}
			/* oneOf is not supported by the openapi generator so marshal and unmarshal the uploadrequest based on the type */
			switch ir.ImageType {
			case ImageTypesAws:
				fallthrough
			case ImageTypesAwsRhui:
				fallthrough
			case ImageTypesAwsHaRhui:
				fallthrough
			case ImageTypesAwsSapRhui:
				var awsUploadOptions AWSEC2UploadOptions
				jsonUploadOptions, err := json.Marshal(*ir.UploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONMarshallingError)
				}
				err = json.Unmarshal(jsonUploadOptions, &awsUploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONUnMarshallingError)
				}

				// For service maintenance, images are discovered by the "Name:composer-api-*"
				// tag filter. Currently all image names in the service are generated, so they're
				// guaranteed to be unique as well. If users are ever allowed to name their images,
				// an extra tag should be added.
				key := fmt.Sprintf("composer-api-%s", uuid.New().String())
				t := target.NewAWSTarget(&target.AWSTargetOptions{
					Filename:          imageType.Filename(),
					Region:            awsUploadOptions.Region,
					Bucket:            h.server.awsBucket,
					Key:               key,
					ShareWithAccounts: awsUploadOptions.ShareWithAccounts,
				})
				if awsUploadOptions.SnapshotName != nil {
					t.ImageName = *awsUploadOptions.SnapshotName
				} else {
					t.ImageName = key
				}

				irTarget = t
			case ImageTypesGuestImage:
				fallthrough
			case ImageTypesVsphere:
				fallthrough
			case ImageTypesImageInstaller:
				fallthrough
			case ImageTypesEdgeInstaller:
				fallthrough
			case ImageTypesEdgeContainer:
				fallthrough
			case ImageTypesEdgeCommit:
				var awsS3UploadOptions AWSS3UploadOptions
				jsonUploadOptions, err := json.Marshal(*ir.UploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONMarshallingError)
				}
				err = json.Unmarshal(jsonUploadOptions, &awsS3UploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONUnMarshallingError)
				}

				key := fmt.Sprintf("composer-api-%s", uuid.New().String())
				t := target.NewAWSS3Target(&target.AWSS3TargetOptions{
					Filename: imageType.Filename(),
					Region:   awsS3UploadOptions.Region,
					Bucket:   h.server.awsBucket,
					Key:      key,
				})
				t.ImageName = key

				irTarget = t
			case ImageTypesGcp:
				var gcpUploadOptions GCPUploadOptions
				jsonUploadOptions, err := json.Marshal(*ir.UploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONMarshallingError)
				}
				err = json.Unmarshal(jsonUploadOptions, &gcpUploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONUnMarshallingError)
				}

				var share []string
				if gcpUploadOptions.ShareWithAccounts != nil {
					share = *gcpUploadOptions.ShareWithAccounts
				}

				object := fmt.Sprintf("composer-api-%s", uuid.New().String())
				t := target.NewGCPTarget(&target.GCPTargetOptions{
					Filename:          imageType.Filename(),
					Region:            gcpUploadOptions.Region,
					Os:                "", // not exposed in cloudapi for now
					Bucket:            gcpUploadOptions.Bucket,
					Object:            object,
					ShareWithAccounts: share,
				})
				// Import will fail if an image with this name already exists
				if gcpUploadOptions.ImageName != nil {
					t.ImageName = *gcpUploadOptions.ImageName
				} else {
					t.ImageName = object
				}

				irTarget = t
			case ImageTypesAzure:
				var azureUploadOptions AzureUploadOptions
				jsonUploadOptions, err := json.Marshal(*ir.UploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONMarshallingError)
				}
				err = json.Unmarshal(jsonUploadOptions, &azureUploadOptions)
				if err != nil {
					return HTTPError(ErrorJSONUnMarshallingError)
				}
				t := target.NewAzureImageTarget(&target.AzureImageTargetOptions{
					Filename:       imageType.Filename(),
					TenantID:       azureUploadOptions.TenantId,
					Location:       azureUploadOptions.Location,
					SubscriptionID: azureUploadOptions.SubscriptionId,
					ResourceGroup:  azureUploadOptions.ResourceGroup,
				})

				if azureUploadOptions.ImageName != nil {
					t.ImageName = *azureUploadOptions.ImageName
				} else {
					// if ImageName wasn't given, generate a random one
					t.ImageName = fmt.Sprintf("composer-api-%s", uuid.New().String())
				}

				irTarget = t
			default:
				return HTTPError(ErrorUnsupportedImageType)
			}
		}

		irs = append(irs, imageRequest{
			imageType:               imageType,
			arch:                    arch,
			repositories:            repositories,
			imageOptions:            imageOptions,
			packageSetsRepositories: packageSetsRepositories,
			target:                  irTarget,
		})
	}

	var id uuid.UUID
	if request.Koji != nil {
		id, err = enqueueKojiCompose(h.server.workers, uint64(request.Koji.TaskId), request.Koji.Server, request.Koji.Name, request.Koji.Version, request.Koji.Release, distribution, bp, manifestSeed, irs)
		if err != nil {
			return err
		}
	} else {
		id, err = enqueueCompose(h.server.workers, distribution, bp, manifestSeed, irs)
		if err != nil {
			return err
		}
	}

	ctx.Logger().Infof("Job ID %s enqueued for operationID %s", id, ctx.Get("operationID"))

	return ctx.JSON(http.StatusCreated, &ComposeId{
		ObjectReference: ObjectReference{
			Href: "/api/image-builder-composer/v2/compose",
			Id:   id.String(),
			Kind: "ComposeId",
		},
		Id: id.String(),
	})
}

func enqueueCompose(workers *worker.Server, distribution distro.Distro, bp blueprint.Blueprint, manifestSeed int64, irs []imageRequest) (uuid.UUID, error) {
	var id uuid.UUID
	if len(irs) != 1 {
		return id, HTTPError(ErrorInvalidNumberOfImageBuilds)
	}
	ir := irs[0]

	depsolveJobID, err := workers.EnqueueDepsolve(&worker.DepsolveJob{
		PackageSets:             ir.imageType.PackageSets(bp),
		Repos:                   ir.repositories,
		ModulePlatformID:        distribution.ModulePlatformID(),
		Arch:                    ir.arch.Name(),
		Releasever:              distribution.Releasever(),
		PackageSetsRepositories: ir.packageSetsRepositories,
	})
	if err != nil {
		return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
	}

	manifestJobID, err := workers.EnqueueManifestJobByID(&worker.ManifestJobByID{}, depsolveJobID)
	if err != nil {
		return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
	}

	id, err = workers.EnqueueOSBuildAsDependency(ir.arch.Name(), &worker.OSBuildJob{
		Targets: []*target.Target{ir.target},
		Exports: ir.imageType.Exports(),
		PipelineNames: &worker.PipelineNames{
			Build:   ir.imageType.BuildPipelines(),
			Payload: ir.imageType.PayloadPipelines(),
		},
	}, manifestJobID)
	if err != nil {
		return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
	go generateManifest(ctx, cancel, workers, depsolveJobID, manifestJobID, ir.imageType, ir.repositories, ir.imageOptions, manifestSeed, bp.Customizations)

	return id, nil
}

func enqueueKojiCompose(workers *worker.Server, taskID uint64, server, name, version, release string, distribution distro.Distro, bp blueprint.Blueprint, manifestSeed int64, irs []imageRequest) (uuid.UUID, error) {
	var id uuid.UUID
	kojiDirectory := "osbuild-composer-koji-" + uuid.New().String()

	initID, err := workers.EnqueueKojiInit(&worker.KojiInitJob{
		Server:  server,
		Name:    name,
		Version: version,
		Release: release,
	})
	if err != nil {
		return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
	}

	var kojiFilenames []string
	var buildIDs []uuid.UUID
	for _, ir := range irs {
		depsolveJobID, err := workers.EnqueueDepsolve(&worker.DepsolveJob{
			PackageSets:             ir.imageType.PackageSets(bp),
			Repos:                   ir.repositories,
			ModulePlatformID:        distribution.ModulePlatformID(),
			Arch:                    ir.arch.Name(),
			Releasever:              distribution.Releasever(),
			PackageSetsRepositories: ir.packageSetsRepositories,
		})
		if err != nil {
			return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
		}

		manifestJobID, err := workers.EnqueueManifestJobByID(&worker.ManifestJobByID{}, depsolveJobID)
		if err != nil {
			return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
		}
		kojiFilename := fmt.Sprintf(
			"%s-%s-%s.%s%s",
			name,
			version,
			release,
			ir.arch.Name(),
			splitExtension(ir.imageType.Filename()),
		)
		buildID, err := workers.EnqueueOSBuildKojiAsDependency(ir.arch.Name(), &worker.OSBuildKojiJob{
			ImageName: ir.imageType.Filename(),
			Exports:   ir.imageType.Exports(),
			PipelineNames: &worker.PipelineNames{
				Build:   ir.imageType.BuildPipelines(),
				Payload: ir.imageType.PayloadPipelines(),
			},
			KojiServer:    server,
			KojiDirectory: kojiDirectory,
			KojiFilename:  kojiFilename,
		}, manifestJobID, initID)
		if err != nil {
			return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
		}
		kojiFilenames = append(kojiFilenames, kojiFilename)
		buildIDs = append(buildIDs, buildID)

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
		go generateManifest(ctx, cancel, workers, depsolveJobID, manifestJobID, ir.imageType, ir.repositories, ir.imageOptions, manifestSeed, bp.Customizations)
	}
	id, err = workers.EnqueueKojiFinalize(&worker.KojiFinalizeJob{
		Server:        server,
		Name:          name,
		Version:       version,
		Release:       release,
		KojiFilenames: kojiFilenames,
		KojiDirectory: kojiDirectory,
		TaskID:        taskID,
		StartTime:     uint64(time.Now().Unix()),
	}, initID, buildIDs)
	if err != nil {
		return id, HTTPErrorWithInternal(ErrorEnqueueingJob, err)
	}

	return id, nil
}

func generateManifest(ctx context.Context, cancel context.CancelFunc, workers *worker.Server, depsolveJobID uuid.UUID, manifestJobID uuid.UUID, imageType distro.ImageType, repos []rpmmd.RepoConfig, options distro.ImageOptions, seed int64, b *blueprint.Customizations) {
	defer cancel()

	// wait until job is in a pending state
	var token uuid.UUID
	var dynArgs []json.RawMessage
	var err error
	for {
		_, token, _, _, dynArgs, err = workers.RequestJobById(ctx, "", manifestJobID)
		if err == jobqueue.ErrNotPending {
			logrus.Debugf("Manifest job %v not pending, waiting for depsolve job to finish", manifestJobID)
			time.Sleep(time.Millisecond * 50)
			select {
			case <-ctx.Done():
				logrus.Warnf("Manifest job %v's dependencies took longer than 5 minutes to finish, returning to avoid dangling routines", manifestJobID)
				break
			default:
				continue
			}
		}
		if err != nil {
			logrus.Errorf("Error requesting manifest job: %v", err)
			return
		}
		break
	}

	var jobResult *worker.ManifestJobByIDResult = &worker.ManifestJobByIDResult{
		Manifest: nil,
	}

	defer func() {
		if jobResult.JobError != nil {
			logrus.Errorf("Error in manifest job %v: %v", manifestJobID, jobResult.JobError.Reason)
		}

		result, err := json.Marshal(jobResult)
		if err != nil {
			logrus.Errorf("Error marshalling manifest job %v results: %v", manifestJobID, err)
		}

		err = workers.FinishJob(token, result)
		if err != nil {
			logrus.Errorf("Error finishing manifest job: %v", err)
		}
	}()

	if len(dynArgs) == 0 {
		reason := "No dynamic arguments"
		jobResult.JobError = clienterrors.WorkerClientError(clienterrors.ErrorNoDynamicArgs, reason)
		return
	}

	var depsolveResults worker.DepsolveJobResult
	err = json.Unmarshal(dynArgs[0], &depsolveResults)
	if err != nil {
		reason := "Error parsing dynamic arguments"
		jobResult.JobError = clienterrors.WorkerClientError(clienterrors.ErrorParsingDynamicArgs, reason)
		return
	}

	_, _, err = workers.DepsolveJobStatus(depsolveJobID, &depsolveResults)
	if err != nil {
		reason := "Error reading depsolve status"
		jobResult.JobError = clienterrors.WorkerClientError(clienterrors.ErrorReadingJobStatus, reason)
		return
	}

	if jobErr := depsolveResults.JobError; jobErr != nil {
		if jobErr.ID == clienterrors.ErrorDNFDepsolveError || jobErr.ID == clienterrors.ErrorDNFMarkingError {
			jobResult.JobError = clienterrors.WorkerClientError(clienterrors.ErrorDepsolveDependency, "Error in depsolve job dependency input, bad package set requested")
			return
		}
		jobResult.JobError = clienterrors.WorkerClientError(clienterrors.ErrorDepsolveDependency, "Error in depsolve job dependency")
		return
	}

	manifest, err := imageType.Manifest(b, options, repos, depsolveResults.PackageSpecs, seed)
	if err != nil {
		reason := "Error generating manifest"
		jobResult.JobError = clienterrors.WorkerClientError(clienterrors.ErrorManifestGeneration, reason)
		return
	}

	jobResult.Manifest = manifest
}

func imageTypeFromApiImageType(it ImageTypes) string {
	switch it {
	case ImageTypesAws:
		return "ami"
	case ImageTypesAwsRhui:
		return "ec2"
	case ImageTypesAwsHaRhui:
		return "ec2-ha"
	case ImageTypesAwsSapRhui:
		return "ec2-sap"
	case ImageTypesGcp:
		return "vhd"
	case ImageTypesAzure:
		return "vhd"
	case ImageTypesGuestImage:
		return "qcow2"
	case ImageTypesVsphere:
		return "vmdk"
	case ImageTypesImageInstaller:
		return "image-installer"
	case ImageTypesEdgeCommit:
		return "rhel-edge-commit"
	case ImageTypesEdgeContainer:
		return "rhel-edge-container"
	case ImageTypesEdgeInstaller:
		return "rhel-edge-installer"
	}
	return ""
}

func (h *apiHandlers) GetComposeStatus(ctx echo.Context, id string) error {
	jobId, err := uuid.Parse(id)
	if err != nil {
		return HTTPError(ErrorInvalidComposeId)
	}

	jobType, err := h.server.workers.JobType(jobId)
	if err != nil {
		return HTTPError(ErrorComposeNotFound)
	}

	if jobType == "osbuild" {
		var result worker.OSBuildJobResult
		status, _, err := h.server.workers.OSBuildJobStatus(jobId, &result)
		if err != nil {
			return HTTPError(ErrorMalformedOSBuildJobResult)
		}

		var us *UploadStatus
		if result.TargetResults != nil {
			// Only single upload target is allowed, therefore only a single upload target result is allowed as well
			if len(result.TargetResults) != 1 {
				return HTTPError(ErrorSeveralUploadTargets)
			}
			tr := *result.TargetResults[0]

			var uploadType UploadTypes
			var uploadOptions interface{}

			switch tr.Name {
			case "org.osbuild.aws":
				uploadType = UploadTypesAws
				awsOptions := tr.Options.(*target.AWSTargetResultOptions)
				uploadOptions = AWSEC2UploadStatus{
					Ami:    awsOptions.Ami,
					Region: awsOptions.Region,
				}
			case "org.osbuild.aws.s3":
				uploadType = UploadTypesAwsS3
				awsOptions := tr.Options.(*target.AWSS3TargetResultOptions)
				uploadOptions = AWSS3UploadStatus{
					Url: awsOptions.URL,
				}
			case "org.osbuild.gcp":
				uploadType = UploadTypesGcp
				gcpOptions := tr.Options.(*target.GCPTargetResultOptions)
				uploadOptions = GCPUploadStatus{
					ImageName: gcpOptions.ImageName,
					ProjectId: gcpOptions.ProjectID,
				}
			case "org.osbuild.azure.image":
				uploadType = UploadTypesAzure
				gcpOptions := tr.Options.(*target.AzureImageTargetResultOptions)
				uploadOptions = AzureUploadStatus{
					ImageName: gcpOptions.ImageName,
				}
			default:
				return HTTPError(ErrorUnknownUploadTarget)
			}

			us = &UploadStatus{
				Status:  UploadStatusValue(result.UploadStatus),
				Type:    uploadType,
				Options: uploadOptions,
			}
		}

		return ctx.JSON(http.StatusOK, ComposeStatus{
			ObjectReference: ObjectReference{
				Href: fmt.Sprintf("/api/image-builder-composer/v2/composes/%v", jobId),
				Id:   jobId.String(),
				Kind: "ComposeStatus",
			},
			Status: composeStatusFromOSBuildJobStatus(status, &result),
			ImageStatus: ImageStatus{
				Status:       imageStatusFromOSBuildJobStatus(status, &result),
				UploadStatus: us,
			},
		})
	} else if jobType == "koji-finalize" {
		var result worker.KojiFinalizeJobResult
		finalizeStatus, deps, err := h.server.workers.KojiFinalizeJobStatus(jobId, &result)
		if err != nil {
			return HTTPError(ErrorMalformedOSBuildJobResult)
		}
		if len(deps) < 2 {
			return HTTPError(ErrorUnexpectedNumberOfImageBuilds)
		}
		var initResult worker.KojiInitJobResult
		_, _, err = h.server.workers.KojiInitJobStatus(deps[0], &initResult)
		if err != nil {
			return HTTPError(ErrorMalformedOSBuildJobResult)
		}
		var buildJobResults []worker.OSBuildKojiJobResult
		var buildJobStatuses []ImageStatus
		for i := 1; i < len(deps); i++ {
			var buildJobResult worker.OSBuildKojiJobResult
			buildJobStatus, _, err := h.server.workers.OSBuildKojiJobStatus(deps[i], &buildJobResult)
			if err != nil {
				return HTTPError(ErrorMalformedOSBuildJobResult)
			}
			buildJobResults = append(buildJobResults, buildJobResult)
			buildJobStatuses = append(buildJobStatuses, ImageStatus{
				Status: imageStatusFromKojiJobStatus(buildJobStatus, &initResult, &buildJobResult),
			})
		}
		response := ComposeStatus{
			ObjectReference: ObjectReference{
				Href: fmt.Sprintf("/api/image-builder-composer/v2/composes/%v", jobId),
				Id:   jobId.String(),
				Kind: "ComposeStatus",
			},
			Status:        composeStatusFromKojiJobStatus(finalizeStatus, &initResult, buildJobResults, &result),
			ImageStatus:   buildJobStatuses[0], // backwards compatibility
			ImageStatuses: &buildJobStatuses,
			KojiStatus:    &KojiStatus{},
		}
		buildID := int(initResult.BuildID)
		if buildID != 0 {
			response.KojiStatus.BuildId = &buildID
		}
		return ctx.JSON(http.StatusOK, response)
	} else {
		return HTTPError(ErrorInvalidJobType)
	}
}

func imageStatusFromOSBuildJobStatus(js *worker.JobStatus, result *worker.OSBuildJobResult) ImageStatusValue {
	if js.Canceled {
		return ImageStatusValueFailure
	}

	if js.Started.IsZero() {
		return ImageStatusValuePending
	}

	if js.Finished.IsZero() {
		// TODO: handle also ImageStatusValueUploading
		// TODO: handle also ImageStatusValueRegistering
		return ImageStatusValueBuilding
	}

	if result.Success {
		return ImageStatusValueSuccess
	}

	return ImageStatusValueFailure
}

func imageStatusFromKojiJobStatus(js *worker.JobStatus, initResult *worker.KojiInitJobResult, buildResult *worker.OSBuildKojiJobResult) ImageStatusValue {
	if js.Canceled {
		return ImageStatusValueFailure
	}

	if initResult.JobError != nil {
		return ImageStatusValueFailure
	}

	if js.Started.IsZero() {
		return ImageStatusValuePending
	}

	if js.Finished.IsZero() {
		return ImageStatusValueBuilding
	}

	if buildResult.JobError != nil {
		return ImageStatusValueFailure
	}

	return ImageStatusValueSuccess
}

func composeStatusFromOSBuildJobStatus(js *worker.JobStatus, result *worker.OSBuildJobResult) ComposeStatusValue {
	if js.Canceled {
		return ComposeStatusValueFailure
	}

	if js.Finished.IsZero() {
		return ComposeStatusValuePending
	}

	if result.Success {
		return ComposeStatusValueSuccess
	}

	return ComposeStatusValueFailure
}

func composeStatusFromKojiJobStatus(js *worker.JobStatus, initResult *worker.KojiInitJobResult, buildResults []worker.OSBuildKojiJobResult, result *worker.KojiFinalizeJobResult) ComposeStatusValue {
	if js.Canceled {
		return ComposeStatusValueFailure
	}

	if js.Finished.IsZero() {
		return ComposeStatusValuePending
	}

	if initResult.JobError != nil {
		return ComposeStatusValueFailure
	}

	for _, buildResult := range buildResults {
		if buildResult.JobError != nil {
			return ComposeStatusValueFailure
		}
	}

	if result.JobError != nil {
		return ComposeStatusValueFailure
	}

	return ComposeStatusValueSuccess
}

// ComposeMetadata handles a /composes/{id}/metadata GET request
func (h *apiHandlers) GetComposeMetadata(ctx echo.Context, id string) error {
	jobId, err := uuid.Parse(id)
	if err != nil {
		return HTTPError(ErrorInvalidComposeId)
	}

	jobType, err := h.server.workers.JobType(jobId)
	if err != nil {
		return HTTPError(ErrorComposeNotFound)
	}

	// TODO: support koji builds
	if jobType != "osbuild" {
		return HTTPError(ErrorInvalidJobType)
	}

	var result worker.OSBuildJobResult
	status, _, err := h.server.workers.OSBuildJobStatus(jobId, &result)
	if err != nil {
		return HTTPErrorWithInternal(ErrorComposeNotFound, err)
	}

	var job worker.OSBuildJob
	if err = h.server.workers.OSBuildJob(jobId, &job); err != nil {
		return HTTPErrorWithInternal(ErrorComposeNotFound, err)
	}

	if status.Finished.IsZero() {
		// job still running: empty response
		return ctx.JSON(200, ComposeMetadata{
			ObjectReference: ObjectReference{
				Href: fmt.Sprintf("/api/image-builder-composer/v2/%v/metadata", jobId),
				Id:   jobId.String(),
				Kind: "ComposeMetadata",
			},
		})
	}

	if status.Canceled || !result.Success {
		// job canceled or failed, empty response
		return ctx.JSON(200, ComposeMetadata{
			ObjectReference: ObjectReference{
				Href: fmt.Sprintf("/api/image-builder-composer/v2/%v/metadata", jobId),
				Id:   jobId.String(),
				Kind: "ComposeMetadata",
			},
		})
	}

	if result.OSBuildOutput == nil || len(result.OSBuildOutput.Log) == 0 {
		// no osbuild output recorded for job, error
		return HTTPError(ErrorMalformedOSBuildJobResult)
	}

	var ostreeCommitMetadata *osbuild.OSTreeCommitStageMetadata
	var rpmStagesMd []osbuild.RPMStageMetadata // collect rpm stage metadata from payload pipelines
	for _, plName := range job.PipelineNames.Payload {
		plMd, hasMd := result.OSBuildOutput.Metadata[plName]
		if !hasMd {
			continue
		}
		for _, stageMd := range plMd {
			switch md := stageMd.(type) {
			case *osbuild.RPMStageMetadata:
				rpmStagesMd = append(rpmStagesMd, *md)
			case *osbuild.OSTreeCommitStageMetadata:
				ostreeCommitMetadata = md
			}
		}
	}

	packages := stagesToPackageMetadata(rpmStagesMd)

	resp := &ComposeMetadata{
		ObjectReference: ObjectReference{
			Href: fmt.Sprintf("/api/image-builder-composer/v2/composes/%v/metadata", jobId),
			Id:   jobId.String(),
			Kind: "ComposeMetadata",
		},
		Packages: &packages,
	}

	if ostreeCommitMetadata != nil {
		resp.OstreeCommit = &ostreeCommitMetadata.Compose.OSTreeCommit
	}

	return ctx.JSON(200, resp)
}

func stagesToPackageMetadata(stages []osbuild.RPMStageMetadata) []PackageMetadata {
	packages := make([]PackageMetadata, 0)
	for _, md := range stages {
		for _, rpm := range md.Packages {
			packages = append(packages,
				PackageMetadata{
					Type:      "rpm",
					Name:      rpm.Name,
					Version:   rpm.Version,
					Release:   rpm.Release,
					Epoch:     rpm.Epoch,
					Arch:      rpm.Arch,
					Sigmd5:    rpm.SigMD5,
					Signature: rpmmd.PackageMetadataToSignature(rpm),
				},
			)
		}
	}
	return packages
}

// Get logs for a compose
func (h *apiHandlers) GetComposeLogs(ctx echo.Context, id string) error {
	jobId, err := uuid.Parse(id)
	if err != nil {
		return HTTPError(ErrorInvalidComposeId)
	}

	jobType, err := h.server.workers.JobType(jobId)
	if err != nil {
		return HTTPError(ErrorComposeNotFound)
	}

	// TODO: support non-koji builds
	if jobType != "koji-finalize" {
		return HTTPError(ErrorInvalidJobType)
	}

	var finalizeResult worker.KojiFinalizeJobResult
	_, deps, err := h.server.workers.KojiFinalizeJobStatus(jobId, &finalizeResult)
	if err != nil {
		return HTTPErrorWithInternal(ErrorComposeNotFound, err)
	}

	var initResult worker.KojiInitJobResult
	_, _, err = h.server.workers.KojiInitJobStatus(deps[0], &initResult)
	if err != nil {
		return HTTPErrorWithInternal(ErrorComposeNotFound, err)
	}

	var buildResultBlobs []interface{}
	for i := 1; i < len(deps); i++ {
		var buildResult worker.OSBuildKojiJobResult
		_, _, err = h.server.workers.OSBuildKojiJobStatus(deps[i], &buildResult)
		if err != nil {
			return HTTPErrorWithInternal(ErrorComposeNotFound, err)
		}
		buildResultBlobs = append(buildResultBlobs, buildResult)
	}

	// Return the OSBuildJobResults as-is for now. The contents of ImageLogs
	// is not part of the API. It's meant for a human to be able to access
	// the logs, which just happen to be in JSON.
	resp := &ComposeLogs{
		ObjectReference: ObjectReference{
			Href: fmt.Sprintf("/api/image-builder-composer/v2/composes/%v/logs", jobId),
			Id:   jobId.String(),
			Kind: "ComposeLogs",
		},
		Koji: &KojiLogs{
			Init:   initResult,
			Import: finalizeResult,
		},
		ImageBuilds: buildResultBlobs,
	}

	return ctx.JSON(http.StatusOK, resp)
}

// GetComposeIdManifests returns the Manifests for a given Compose (one for each image).
func (h *apiHandlers) GetComposeManifests(ctx echo.Context, id string) error {
	jobId, err := uuid.Parse(id)
	if err != nil {
		return HTTPError(ErrorInvalidComposeId)
	}

	jobType, err := h.server.workers.JobType(jobId)
	if err != nil {
		return HTTPError(ErrorComposeNotFound)
	}

	// TODO: support non-koji builds
	if jobType != "koji-finalize" {
		return HTTPError(ErrorInvalidJobType)
	}

	var finalizeResult worker.KojiFinalizeJobResult
	_, deps, err := h.server.workers.KojiFinalizeJobStatus(jobId, &finalizeResult)
	if err != nil {
		return HTTPErrorWithInternal(ErrorComposeNotFound, err)
	}

	var manifestBlobs []interface{}
	for _, id := range deps[1:] {
		var buildJob worker.OSBuildKojiJob
		err = h.server.workers.OSBuildKojiJob(id, &buildJob)
		if err != nil {
			return HTTPErrorWithInternal(ErrorComposeNotFound, err)
		}
		var manifest distro.Manifest
		if len(buildJob.Manifest) == 0 {
			manifest = buildJob.Manifest
		} else {
			_, deps, err := h.server.workers.OSBuildKojiJobStatus(id, nil)
			if err != nil {
				return HTTPErrorWithInternal(ErrorComposeNotFound, err)
			}
			if len(deps) < 2 {
				return HTTPErrorWithInternal(ErrorComposeNotFound, err)
			}
			var manifestResult worker.ManifestJobByIDResult
			_, _, err = h.server.workers.ManifestJobStatus(deps[1], &manifestResult)
			if err != nil {
				return HTTPErrorWithInternal(ErrorComposeNotFound, err)
			}
			manifest = manifestResult.Manifest
		}
		manifestBlobs = append(manifestBlobs, manifest)
	}

	resp := &ComposeManifests{
		ObjectReference: ObjectReference{
			Href: fmt.Sprintf("/api/image-builder-composer/v2/composes/%v/manifests", jobId),
			Id:   jobId.String(),
			Kind: "ComposeManifests",
		},
		Manifests: manifestBlobs,
	}

	return ctx.JSON(http.StatusOK, resp)
}
