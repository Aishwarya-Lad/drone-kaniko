package main

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"io/ioutil"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecrpublic"
	"github.com/aws/smithy-go"
	kaniko "github.com/drone/drone-kaniko"
	"github.com/drone/drone-kaniko/cmd/artifact"
	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	accessKeyEnv     string = "AWS_ACCESS_KEY_ID"
	secretKeyEnv     string = "AWS_SECRET_ACCESS_KEY"
	dockerConfigPath string = "/kaniko/.docker/config.json"
	ecrPublicDomain  string = "public.ecr.aws"

	defaultDigestFile string = "/kaniko/digest-file"
)

var (
	version = "unknown"
)

func main() {
	// Load env-file if it exists first
	if env := os.Getenv("PLUGIN_ENV_FILE"); env != "" {
		if err := godotenv.Load(env); err != nil {
			logrus.Fatal(err)
		}
	}

	app := cli.NewApp()
	app.Name = "kaniko docker plugin"
	app.Usage = "kaniko docker plugin"
	app.Action = run
	app.Version = version
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "dockerfile",
			Usage:  "build dockerfile",
			Value:  "Dockerfile",
			EnvVar: "PLUGIN_DOCKERFILE",
		},
		cli.StringFlag{
			Name:   "context",
			Usage:  "build context",
			Value:  ".",
			EnvVar: "PLUGIN_CONTEXT",
		},
		cli.StringSliceFlag{
			Name:     "tags",
			Usage:    "build tags",
			Value:    &cli.StringSlice{"latest"},
			EnvVar:   "PLUGIN_TAGS",
			FilePath: ".tags",
		},
		cli.StringSliceFlag{
			Name:   "args",
			Usage:  "build args",
			EnvVar: "PLUGIN_BUILD_ARGS",
		},
		cli.StringFlag{
			Name:   "target",
			Usage:  "build target",
			EnvVar: "PLUGIN_TARGET",
		},
		cli.StringFlag{
			Name:   "repo",
			Usage:  "docker repository",
			EnvVar: "PLUGIN_REPO",
		},
		cli.BoolFlag{
			Name:   "create-repository",
			Usage:  "create ECR repository",
			EnvVar: "PLUGIN_CREATE_REPOSITORY",
		},
		cli.StringFlag{
			Name:   "region",
			Usage:  "AWS region",
			Value:  "us-east-1",
			EnvVar: "PLUGIN_REGION",
		},
		cli.StringSliceFlag{
			Name:   "custom-labels",
			Usage:  "additional k=v labels",
			EnvVar: "PLUGIN_CUSTOM_LABELS",
		},
		cli.StringFlag{
			Name:   "registry",
			Usage:  "ECR registry",
			EnvVar: "PLUGIN_REGISTRY",
		},
		cli.StringFlag{
			Name:   "access-key",
			Usage:  "ECR access key",
			EnvVar: "PLUGIN_ACCESS_KEY",
		},
		cli.StringFlag{
			Name:   "secret-key",
			Usage:  "ECR secret key",
			EnvVar: "PLUGIN_SECRET_KEY",
		},
		cli.StringFlag{
			Name:   "snapshot-mode",
			Usage:  "Specify one of full, redo or time as snapshot mode",
			EnvVar: "PLUGIN_SNAPSHOT_MODE",
		},
		cli.StringFlag{
			Name:   "lifecycle-policy",
			Usage:  "Path to lifecycle policy file",
			EnvVar: "PLUGIN_LIFECYCLE_POLICY",
		},
		cli.StringFlag{
			Name:   "repository-policy",
			Usage:  "Path to repository policy file",
			EnvVar: "PLUGIN_REPOSITORY_POLICY",
		},
		cli.BoolFlag{
			Name:   "enable-cache",
			Usage:  "Set this flag to opt into caching with kaniko",
			EnvVar: "PLUGIN_ENABLE_CACHE",
		},
		cli.StringFlag{
			Name:   "cache-repo",
			Usage:  "Remote repository that will be used to store cached layers. Cache repo should be present in specified registry. enable-cache needs to be set to use this flag",
			EnvVar: "PLUGIN_CACHE_REPO",
		},
		cli.IntFlag{
			Name:   "cache-ttl",
			Usage:  "Cache timeout in hours. Defaults to two weeks.",
			EnvVar: "PLUGIN_CACHE_TTL",
		},
		cli.StringFlag{
			Name:   "artifact-file",
			Usage:  "Artifact file location that will be generated by the plugin. This file will include information of docker images that are uploaded by the plugin.",
			EnvVar: "PLUGIN_ARTIFACT_FILE",
		},
		cli.BoolFlag{
			Name:   "no-push",
			Usage:  "Set this flag if you only want to build the image, without pushing to a registry",
			EnvVar: "PLUGIN_NO_PUSH",
		},
		cli.StringFlag{
			Name:   "verbosity",
			Usage:  "Set this flag with value as oneof <panic|fatal|error|warn|info|debug|trace> to set the logging level for kaniko. Defaults to info.",
			EnvVar: "PLUGIN_VERBOSITY",
		},
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func run(c *cli.Context) error {
	repo := c.String("repo")
	registry := c.String("registry")
	region := c.String("region")
	accessKey := c.String("access-key")
	noPush := c.Bool("no-push")

	// only setup auth when pushing or credentials are defined
	if !noPush || accessKey != "" {
		if err := setupECRAuth(accessKey, c.String("secret-key"), registry); err != nil {
			return err
		}
	}

	// only create repository when pushing and create-repository is true
	if !noPush && c.Bool("create-repository") {
		if err := createRepository(region, repo, registry); err != nil {
			return err
		}
	}

	if c.IsSet("lifecycle-policy") {
		contents, err := ioutil.ReadFile(c.String("lifecycle-policy"))
		if err != nil {
			logrus.Fatal(err)
		}
		if err := uploadLifeCyclePolicy(region, repo, string(contents)); err != nil {
			logrus.Fatal(fmt.Sprintf("error uploading ECR lifecycle policy: %v", err))
		}
	}

	if c.IsSet("repository-policy") {
		contents, err := ioutil.ReadFile(c.String("repository-policy"))
		if err != nil {
			logrus.Fatal(err)
		}
		if err := uploadRepositoryPolicy(region, repo, registry, string(contents)); err != nil {
			logrus.Fatal(fmt.Sprintf("error uploading ECR lifecycle policy: %v", err))
		}
	}

	plugin := kaniko.Plugin{
		Build: kaniko.Build{
			Dockerfile:   c.String("dockerfile"),
			Context:      c.String("context"),
			Tags:         c.StringSlice("tags"),
			Args:         c.StringSlice("args"),
			Target:       c.String("target"),
			Repo:         fmt.Sprintf("%s/%s", c.String("registry"), c.String("repo")),
			Labels:       c.StringSlice("custom-labels"),
			SnapshotMode: c.String("snapshot-mode"),
			EnableCache:  c.Bool("enable-cache"),
			CacheRepo:    fmt.Sprintf("%s/%s", c.String("registry"), c.String("cache-repo")),
			CacheTTL:     c.Int("cache-ttl"),
			DigestFile:   defaultDigestFile,
			NoPush:       noPush,
			Verbosity:    c.String("verbosity"),
		},
		Artifact: kaniko.Artifact{
			Tags:         c.StringSlice("tags"),
			Repo:         c.String("repo"),
			Registry:     c.String("registry"),
			ArtifactFile: c.String("artifact-file"),
			RegistryType: artifact.ECR,
		},
	}
	return plugin.Exec()
}

func setupECRAuth(accessKey, secretKey, registry string) error {
	if registry == "" {
		return fmt.Errorf("registry must be specified")
	}

	// If IAM role is used, access key & secret key are not required
	if accessKey != "" && secretKey != "" {
		err := os.Setenv(accessKeyEnv, accessKey)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("failed to set %s environment variable", accessKeyEnv))
		}

		err = os.Setenv(secretKeyEnv, secretKey)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("failed to set %s environment variable", secretKeyEnv))
		}
	}

	jsonBytes := []byte(fmt.Sprintf(`{"credStore": "ecr-login", "credHelpers": {"%s": "ecr-login", "%s": "ecr-login"}}`, ecrPublicDomain, registry))
	err := ioutil.WriteFile(dockerConfigPath, jsonBytes, 0644)
	if err != nil {
		return errors.Wrap(err, "failed to create docker config file")
	}
	return nil
}

func createRepository(region, repo, registry string) error {
	if registry == "" {
		return fmt.Errorf("registry must be specified")
	}

	if repo == "" {
		return fmt.Errorf("repo must be specified")
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		return errors.Wrap(err, "failed to load aws config")
	}

	var createErr error

	//create public repo
	//if registry string starts with public domain (ex: public.ecr.aws/example-registry)
	if isRegistryPublic(registry) {
		svc := ecrpublic.NewFromConfig(cfg)
		_, createErr = svc.CreateRepository(context.TODO(), &ecrpublic.CreateRepositoryInput{RepositoryName: &repo})
		//create private repo
	} else {
		svc := ecr.NewFromConfig(cfg)
		_, createErr = svc.CreateRepository(context.TODO(), &ecr.CreateRepositoryInput{RepositoryName: &repo})
	}

	var apiError smithy.APIError
	if errors.As(createErr, &apiError) && apiError.ErrorCode() != "RepositoryAlreadyExistsException" {
		return errors.Wrap(createErr, "failed to create repository")
	}

	return nil
}

func uploadLifeCyclePolicy(region, repo, lifecyclePolicy string) (err error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		return errors.Wrap(err, "failed to load aws config")
	}

	svc := ecr.NewFromConfig(cfg)

	input := &ecr.PutLifecyclePolicyInput{
		LifecyclePolicyText: aws.String(lifecyclePolicy),
		RepositoryName:      aws.String(repo),
	}
	_, err = svc.PutLifecyclePolicy(context.TODO(), input)

	return err
}

func uploadRepositoryPolicy(region, repo, registry, repositoryPolicy string) (err error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		return errors.Wrap(err, "failed to load aws config")
	}

	if isRegistryPublic(registry) {
		svc := ecrpublic.NewFromConfig(cfg)

		input := &ecrpublic.SetRepositoryPolicyInput{
			PolicyText:     aws.String(repositoryPolicy),
			RepositoryName: aws.String(repo),
		}
		_, err = svc.SetRepositoryPolicy(context.TODO(), input)
	} else {

		svc := ecr.NewFromConfig(cfg)

		input := &ecr.SetRepositoryPolicyInput{
			PolicyText:     aws.String(repositoryPolicy),
			RepositoryName: aws.String(repo),
		}
		_, err = svc.SetRepositoryPolicy(context.TODO(), input)
	}

	return err
}

func isRegistryPublic(registry string) bool {
	return strings.HasPrefix(registry, ecrPublicDomain)
}
