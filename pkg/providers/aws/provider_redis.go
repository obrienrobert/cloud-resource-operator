package aws

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aws/aws-sdk-go/service/elasticache/elasticacheiface"
	"github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elasticache"
	"github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/cloud-resource-operator/pkg/resources"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/integr8ly/cloud-resource-operator/pkg/providers"

	errorUtil "github.com/pkg/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultCacheNodeType     = "cache.t2.micro"
	defaultEngineVersion     = "3.2.10"
	defaultDescription       = "A Redis replication group"
	defaultNumCacheClusters  = 2
	defaultSnapshotRetention = 30
)

// AWS Redis Provider implementation for AWS Elasticache
type AWSRedisProvider struct {
	Client            client.Client
	Logger            *logrus.Entry
	CredentialManager CredentialManager
	ConfigManager     ConfigManager
	CacheSvc          elasticacheiface.ElastiCacheAPI
}

func NewAWSRedisProvider(client client.Client, logger *logrus.Entry) *AWSRedisProvider {
	return &AWSRedisProvider{
		Client:            client,
		Logger:            logger.WithFields(logrus.Fields{"provider": "aws_redis"}),
		CredentialManager: NewCredentialMinterCredentialManager(client),
		ConfigManager:     NewDefaultConfigMapConfigManager(client),
	}
}

func (p *AWSRedisProvider) GetName() string {
	return providers.AWSDeploymentStrategy
}

func (p *AWSRedisProvider) SupportsStrategy(d string) bool {
	return d == providers.AWSDeploymentStrategy
}

// CreateRedis Create an Elasticache Replication Group from strategy config
func (p *AWSRedisProvider) CreateRedis(ctx context.Context, r *v1alpha1.Redis) (*providers.RedisCluster, error) {
	// handle provider-specific finalizer
	if r.GetDeletionTimestamp() == nil {
		resources.AddFinalizer(&r.ObjectMeta, DefaultFinalizer)
		if err := p.Client.Update(ctx, r); err != nil {
			return nil, errorUtil.Wrapf(err, "failed to add finalizer to instance")
		}
	}

	// info about the redis cluster to be created
	redisConfig, stratCfg, err := p.getRedisConfig(ctx, r)
	if err != nil {
		return nil, errorUtil.Wrapf(err, "failed to retrieve aws redis cluster config for instance %s", r.Name)
	}
	if redisConfig.ReplicationGroupId == nil {
		redisConfig.ReplicationGroupId = aws.String(r.Name)
	}

	// create the credentials to be used by the aws resource providers, not to be used by end-user
	providerCreds, err := p.CredentialManager.ReconcileProviderCredentials(ctx, r.Namespace)
	if err != nil {
		return nil, errorUtil.Wrap(err, "failed to reconcile s3 put object credentials")
	}

	// setup aws redis cluster sdk session
	cacheSvc := createCacheService(stratCfg, providerCreds)

	// create the aws redis cluster
	return createRedisCluster(cacheSvc, redisConfig)
}

func createCacheService(stratCfg *StrategyConfig, providerCreds *AWSCredentials) elasticacheiface.ElastiCacheAPI {
	sess := session.Must(session.NewSession(&aws.Config{
		Region:      aws.String(stratCfg.Region),
		Credentials: credentials.NewStaticCredentials(providerCreds.AccessKeyID, providerCreds.SecretAccessKey, ""),
	}))
	return elasticache.New(sess)
}

func createRedisCluster(cacheSvc elasticacheiface.ElastiCacheAPI, redisConfig *elasticache.CreateReplicationGroupInput) (*providers.RedisCluster, error) {
	// the aws access key can sometimes still not be registered in aws on first try, so loop
	rgs, err := getReplicationGroups(cacheSvc)
	if err != nil {
		// return nil error so this function can be requeueed
		logrus.Info("error getting replication groups:", err)
		return nil, err
	}

	// check if the cluster has already been created
	var foundCache *elasticache.ReplicationGroup
	for _, c := range rgs {
		if *c.ReplicationGroupId == *redisConfig.ReplicationGroupId {
			foundCache = c
			break
		}
	}
	if foundCache != nil {
		if *foundCache.Status == "available" {
			logrus.Info("found existing redis cluster")
			primaryEndpoint := foundCache.NodeGroups[0].PrimaryEndpoint
			return &providers.RedisCluster{DeploymentDetails: &providers.RedisDeploymentDetails{
				URI:  *primaryEndpoint.Address,
				Port: *primaryEndpoint.Port,
			}}, nil
		}
		return nil, nil
	}

	// the cluster doesn't exist, so create it
	// verify that all values are set or use defaults
	logrus.Info("creating redis cluster")
	verifyRedisConfig(redisConfig)
	input := &elasticache.CreateReplicationGroupInput{
		AutomaticFailoverEnabled:    aws.Bool(true),
		Engine:                      aws.String("redis"),
		ReplicationGroupId:          redisConfig.ReplicationGroupId,
		CacheNodeType:               redisConfig.CacheNodeType,
		EngineVersion:               redisConfig.EngineVersion,
		ReplicationGroupDescription: redisConfig.ReplicationGroupDescription,
		NumCacheClusters:            redisConfig.NumCacheClusters,
		SnapshotRetentionLimit:      redisConfig.SnapshotRetentionLimit,
	}
	_, err = cacheSvc.CreateReplicationGroup(input)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

// DeleteStorage Delete elasticache replication group
func (p *AWSRedisProvider) DeleteRedis(ctx context.Context, r *v1alpha1.Redis) error {
	// resolve redis information for redis created by provider
	redisConfig, stratCfg, err := p.getRedisConfig(ctx, r)
	if err != nil {
		return errorUtil.Wrapf(err, "failed to retrieve aws redis config for instance %s", r.Name)
	}
	if redisConfig.ReplicationGroupId == nil {
		redisConfig.ReplicationGroupId = aws.String(r.Name)
	}

	// get provider aws creds so the redis cluster can be deleted
	providerCreds, err := p.CredentialManager.ReconcileProviderCredentials(ctx, r.Namespace)
	if err != nil {
		return errorUtil.Wrap(err, "failed to reconcile aws provider credentials")
	}

	// setup aws redis cluster sdk session
	cacheSvc := createCacheService(stratCfg, providerCreds)

	// delete the redis cluster
	return p.deleteRedisCluster(cacheSvc, redisConfig, ctx, r)
}

func (p *AWSRedisProvider) deleteRedisCluster(cacheSvc elasticacheiface.ElastiCacheAPI, redisConfig *elasticache.CreateReplicationGroupInput, ctx context.Context, r *v1alpha1.Redis) error {
	// the aws access key can sometimes still not be registered in aws on first try, so loop
	rgs, err := getReplicationGroups(cacheSvc)
	if err != nil {
		return err
	}

	// check if the cluster has already been deleted
	var foundCache *elasticache.ReplicationGroup
	for _, c := range rgs {
		if *c.ReplicationGroupId == *redisConfig.ReplicationGroupId {
			foundCache = c
			break
		}
	}
	// check if replication group does not exist and delete finalizer
	if foundCache == nil {
		// remove the finalizer added by the provider
		resources.RemoveFinalizer(&r.ObjectMeta, DefaultFinalizer)
		if err := p.Client.Update(ctx, r); err != nil {
			return errorUtil.Wrapf(err, "failed to update instance as part of finalizer reconcile")
		}
		return nil
	}
	// check if replication group exists and is available
	if *foundCache.Status == "available" {
		// delete the redis cluster that was created by the provider
		_, err = cacheSvc.DeleteReplicationGroup(&elasticache.DeleteReplicationGroupInput{
			ReplicationGroupId:   redisConfig.ReplicationGroupId,
			RetainPrimaryCluster: aws.Bool(false),
		})
		redisErr, isAwsErr := err.(awserr.Error)
		if err != nil && !isAwsErr {
			return errorUtil.Wrapf(err, "failed to delete elasticache cluster %s", *redisConfig.ReplicationGroupId)
		}
		if err != nil && isAwsErr {
			if redisErr.Code() != elasticache.ErrCodeReplicationGroupNotFoundFault {
				return errorUtil.Wrapf(err, "failed to delete elasticache cluster %s, aws error", *redisConfig.ReplicationGroupId)
			}
		}
	}
	return nil
}

// poll for replication groups
func getReplicationGroups(cacheSvc elasticacheiface.ElastiCacheAPI) ([]*elasticache.ReplicationGroup, error) {
	var rgs []*elasticache.ReplicationGroup
	err := wait.PollImmediate(time.Second*5, time.Minute*5, func() (done bool, err error) {
		listOutput, err := cacheSvc.DescribeReplicationGroups(&elasticache.DescribeReplicationGroupsInput{})
		if err != nil {
			return false, nil
		}
		rgs = listOutput.ReplicationGroups
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return rgs, nil
}

// getRedisConfig retrieves the redis config from the cloud-resources-aws-strategies configmap
func (p *AWSRedisProvider) getRedisConfig(ctx context.Context, r *v1alpha1.Redis) (*elasticache.CreateReplicationGroupInput, *StrategyConfig, error) {
	stratCfg, err := p.ConfigManager.ReadStorageStrategy(ctx, providers.RedisResourceType, r.Spec.Tier)
	if err != nil {
		return nil, nil, errorUtil.Wrap(err, "failed to read aws strategy config")
	}
	if stratCfg.Region == "" {
		stratCfg.Region = DefaultRegion
	}

	// unmarshal the redis cluster config
	redisConfig := &elasticache.CreateReplicationGroupInput{}
	if err := json.Unmarshal(stratCfg.RawStrategy, redisConfig); err != nil {
		return nil, nil, errorUtil.Wrap(err, "failed to unmarshal aws redis cluster configuration")
	}
	return redisConfig, stratCfg, nil
}

// verifyRedisConfig checks redis config, if none exist sets values to default
func verifyRedisConfig(redisConfig *elasticache.CreateReplicationGroupInput) {
	if redisConfig.CacheNodeType == nil {
		redisConfig.CacheNodeType = aws.String(defaultCacheNodeType)
	}
	if redisConfig.ReplicationGroupDescription == nil {
		redisConfig.ReplicationGroupDescription = aws.String(defaultDescription)
	}
	if redisConfig.EngineVersion == nil {
		redisConfig.EngineVersion = aws.String(defaultEngineVersion)
	}
	if redisConfig.NumCacheClusters == nil {
		redisConfig.NumCacheClusters = aws.Int64(defaultNumCacheClusters)
	}
	if redisConfig.SnapshotRetentionLimit == nil {
		redisConfig.SnapshotRetentionLimit = aws.Int64(defaultSnapshotRetention)
	}
}
