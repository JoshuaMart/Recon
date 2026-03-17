package scaleway

import (
	"fmt"

	jobs "github.com/scaleway/scaleway-sdk-go/api/jobs/v1alpha1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

type Client struct {
	api             *jobs.API
	region          scw.Region
	jobDefinitionID string
	apiBaseURL      string
	ingestAPIKey    string
}

func NewClient(accessKey, secretKey, projectID, region, jobDefinitionID, apiBaseURL, ingestAPIKey string) (*Client, error) {
	scwClient, err := scw.NewClient(
		scw.WithAuth(accessKey, secretKey),
		scw.WithDefaultProjectID(projectID),
		scw.WithDefaultRegion(scw.Region(region)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create scaleway client: %w", err)
	}

	return &Client{
		api:             jobs.NewAPI(scwClient),
		region:          scw.Region(region),
		jobDefinitionID: jobDefinitionID,
		apiBaseURL:      apiBaseURL,
		ingestAPIKey:    ingestAPIKey,
	}, nil
}

func (c *Client) LaunchJob(wildcardValue, jobID, mode string) (string, error) {
	envVars := map[string]string{
		"WILDCARD":       wildcardValue,
		"JOB_ID":         jobID,
		"MODE":           mode,
		"API_URL":        c.apiBaseURL,
		"INGEST_API_KEY": c.ingestAPIKey,
	}

	resp, err := c.api.StartJobDefinition(&jobs.StartJobDefinitionRequest{
		Region:               c.region,
		JobDefinitionID:      c.jobDefinitionID,
		EnvironmentVariables: &envVars,
	})
	if err != nil {
		return "", fmt.Errorf("failed to start scaleway job: %w", err)
	}

	if len(resp.JobRuns) == 0 {
		return "", fmt.Errorf("scaleway returned no job runs")
	}

	return resp.JobRuns[0].ID, nil
}

func (c *Client) GetJobStatus(jobRunID string) (string, error) {
	run, err := c.api.GetJobRun(&jobs.GetJobRunRequest{
		Region:   c.region,
		JobRunID: jobRunID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get scaleway job run: %w", err)
	}

	return string(run.State), nil
}
