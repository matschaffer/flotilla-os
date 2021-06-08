package logs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/pkg/errors"
	"github.com/stitchfix/flotilla-os/config"
	"github.com/stitchfix/flotilla-os/state"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//
// K8SS3LogsClient corresponds with the aws logs driver
// for ECS and returns logs for runs
//
type K8SS3LogsClient struct {
	logRetentionInDays int64
	logNamespace       string
	s3Client           *s3.S3
	s3Bucket           string
	s3BucketRootDir    string
	logger             *log.Logger
}

type s3Log struct {
	Log    string    `json:"log"`
	Stream string    `json:"stream"`
	Time   time.Time `json:"time"`
}

//
// Name returns the name of the logs client
//
func (lc *K8SS3LogsClient) Name() string {
	return "k8s-s3"
}

//
// Initialize sets up the K8SS3LogsClient
//
func (lc *K8SS3LogsClient) Initialize(conf config.Config) error {
	confLogOptions := conf.GetStringMapString("k8s.log.driver.options")

	awsRegion := confLogOptions["awslogs-region"]
	if len(awsRegion) == 0 {
		awsRegion = conf.GetString("aws_default_region")
	}

	if len(awsRegion) == 0 {
		return errors.Errorf(
			"K8SS3LogsClient needs one of [k8s.log.driver.options.awslogs-region] or [aws_default_region] set in config")
	}

	flotillaMode := conf.GetString("flotilla_mode")
	if flotillaMode != "test" {
		sess := session.Must(session.NewSession(&aws.Config{
			Region: aws.String(awsRegion)}))

		lc.s3Client = s3.New(sess, aws.NewConfig().WithRegion(awsRegion))
	}

	s3BucketName := confLogOptions["s3_bucket_name"]

	if len(s3BucketName) == 0 {
		return errors.Errorf(
			"K8SS3LogsClient needs [k8s.log.driver.options.s3_bucket_name] set in config")
	}
	lc.s3Bucket = s3BucketName

	s3BucketRootDir := confLogOptions["s3_bucket_root_dir"]

	if len(s3BucketRootDir) == 0 {
		return errors.Errorf(
			"K8SS3LogsClient needs [k8s.log.driver.options.s3_bucket_root_dir] set in config")
	}
	lc.s3BucketRootDir = s3BucketRootDir

	lc.logger = log.New(os.Stderr, "[s3logs] ",
		log.Ldate|log.Ltime|log.Lshortfile)
	return nil
}

func (lc *K8SS3LogsClient) Logs(executable state.Executable, run state.Run, lastSeen *string) (string, *string, error) {
	result, err := lc.getS3Object(run)
	startPosition := int64(0)
	if lastSeen != nil {
		parsed, err := strconv.ParseInt(*lastSeen, 10, 64)
		if err == nil {
			startPosition = parsed
		}
	}

	if result != nil && err == nil {
		acc, position, err := lc.logsToMessageString(result, startPosition)
		newLastSeen := fmt.Sprintf("%d", position)
		return acc, &newLastSeen, err
	}

	return "", aws.String(""), errors.Errorf("No logs.")
}

//
// Logs returns all logs from the log stream identified by handle since lastSeen
//
func (lc *K8SS3LogsClient) LogsText(executable state.Executable, run state.Run, w http.ResponseWriter) error {
	result, err := lc.getS3Object(run)

	if result != nil && err == nil {
		return lc.logsToMessage(result, w)
	}

	return nil
}

//
// Fetch S3Object associated with the pod's log.
//
func (lc *K8SS3LogsClient) getS3Object(run state.Run) (*s3.GetObjectOutput, error) {
	//Pod isn't there yet - dont return a 404
	if run.PodName == nil {
		return nil, errors.New("no pod associated with the run.")
	}
	s3DirName := lc.toS3DirName(run)

	// Get list of S3 objects in the run_id folder.
	result, err := lc.s3Client.ListObjects(&s3.ListObjectsInput{
		Bucket: aws.String(lc.s3Bucket),
		Prefix: aws.String(s3DirName),
	})

	if err != nil {
		return nil, errors.Wrap(err, "problem getting logs")
	}

	if result == nil || result.Contents == nil || len(result.Contents) == 0 {
		return nil, errors.New("no s3 files associated with the run.")
	}
	var key *string
	lastModified := &time.Time{}

	//Find latest log file (could have multiple log files per pod - due to pod retries)
	for _, content := range result.Contents {
		if strings.Contains(*content.Key, run.RunID) && lastModified.Before(*content.LastModified) {
			key = content.Key
			lastModified = content.LastModified
		}
	}
	if key != nil {
		return lc.getS3Key(key)
	} else {
		return nil, errors.New("no s3 files associated with the run.")
	}
}

func (lc *K8SS3LogsClient) getS3Key(s3Key *string) (*s3.GetObjectOutput, error) {
	result, err := lc.s3Client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(lc.s3Bucket),
		Key:    aws.String(*s3Key),
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

//
// Formulate dir name on S3.
//
func (lc *K8SS3LogsClient) toS3DirName(run state.Run) string {
	return fmt.Sprintf("%s/%s", lc.s3BucketRootDir, run.RunID)
}

//
// Converts log messages from S3 to strings - returns the contents of the entire file.
//
func (lc *K8SS3LogsClient) logsToMessage(result *s3.GetObjectOutput, w http.ResponseWriter) error {
	reader := bufio.NewReader(result.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return err
		} else {
			var parsedLine s3Log
			err := json.Unmarshal(line, &parsedLine)
			if err != nil {
				return err
			}
			_, err = io.WriteString(w, parsedLine.Log)
			if err != nil {
				return err
			}
		}
	}

}

//
// Converts log messages from S3 to strings, takes a starting offset.
//
func (lc *K8SS3LogsClient) logsToMessageString(result *s3.GetObjectOutput, startingPosition int64) (string, int64, error) {
	acc := ""
	currentPosition := int64(0)
	// if less than/equal to 0, read entire log.
	if startingPosition <= 0 {
		startingPosition = currentPosition
	}

	// No S3 file or object, return "", 0, err
	if result == nil {
		return acc, startingPosition, errors.New("s3 object not present.")
	}

	reader := bufio.NewReader(result.Body)

	// Reading until startingPosition and discard unneeded lines.
	for currentPosition < startingPosition {
		currentPosition = currentPosition + 1
		_, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return acc, startingPosition, err
		}
	}

	// Read upto MaxLogLines
	for currentPosition <= startingPosition+state.MaxLogLines {
		currentPosition = currentPosition + 1
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return acc, currentPosition, err
		} else {
			var parsedLine s3Log
			err := json.Unmarshal(line, &parsedLine)
			if err == nil {
				acc = fmt.Sprintf("%s%s", acc, parsedLine.Log)
			}
		}
	}

	_ = result.Body.Close()
	return acc, currentPosition, nil
}