package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/speps/go-hashids/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type ProgressUpdateJobs struct {
	Team                  string
	LastChallengeProgress []ChallengeStatus
}

type ChallengeResponse struct {
	Status string      `json:"status"`
	Data   []Challenge `json:"data"`
}
type Challenge struct {
	Id          int    `json:"id"`
	Name        string `json:"name"`
	Key         string `json:"key"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Difficulty  int    `json:"difficulty"`
	Solved      bool   `json:"solved"`
	UpdatedAt   string `json:"updatedAt"`
}

var challengeIdLookup = map[string]int{}

// JuiceShopChallenge represents a challenge in the Juice Shop config file. reduced to just the key, everything else is not needed
type JuiceShopChallenge struct {
	Key string `json:"key"`
}

// Create a structure to hold all challenge progress including FindIt and FixIt codes
type ChallengeProgressWithCodes struct {
	Challenges         []ChallengeStatus
	ContinueCodeFindIt string
	ContinueCodeFixIt  string
}

func StartBackgroundSync(clientset *kubernetes.Clientset, workerCount int) {
	logger.Printf("Starting background-sync looking for JuiceShop challenge progress changes with %d workers", workerCount)

	createChallengeIdLookup()

	progressUpdateJobs := make(chan ProgressUpdateJobs)

	// Start 10 workers which fetch and update ContinueCodes based on the `progressUpdateJobs` queue / channel
	for i := 0; i < workerCount; i++ {
		go workOnProgressUpdates(progressUpdateJobs, clientset)
	}

	go createProgressUpdateJobs(progressUpdateJobs, clientset)
}

func createChallengeIdLookup() {
	challengesBytes, err := os.ReadFile("/challenges.json")
	if err != nil {
		panic(fmt.Errorf("failed to read challenges.json. This is fatal as the progress watchdog needs it to map between challenge keys and challenge ids: %w", err))
	}

	var challenges []JuiceShopChallenge
	err = json.Unmarshal(challengesBytes, &challenges)
	if err != nil {
		panic(fmt.Errorf("failed to decode challenges.json. This is fatal as the progress watchdog needs it to map between challenge keys and challenge ids: %w", err))
	}

	for i, challenge := range challenges {
		challengeIdLookup[challenge.Key] = i + 1
	}
}

// Constantly lists all JuiceShops in managed by MultiJuicer and queues progressUpdatesJobs for them
func createProgressUpdateJobs(progressUpdateJobs chan<- ProgressUpdateJobs, clientset *kubernetes.Clientset) {
	namespace := os.Getenv("NAMESPACE")
	for {
		// Get Instances
		opts := metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=juice-shop",
		}
		juiceShops, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), opts)
		if err != nil {
			panic(err.Error())
		}

		logger.Printf("Background-sync started syncing %d instances", len(juiceShops.Items))

		for _, instance := range juiceShops.Items {
			Team := instance.Labels["team"]

			if instance.Status.ReadyReplicas != 1 {
				continue
			}

			var lastChallengeProgress []ChallengeStatus
			json.Unmarshal([]byte(instance.Annotations["multi-juicer.owasp-juice.shop/challenges"]), &lastChallengeProgress)

			progressUpdateJobs <- ProgressUpdateJobs{
				Team:                  Team,
				LastChallengeProgress: lastChallengeProgress,
			}
		}
		time.Sleep(60 * time.Second)
	}
}

// Modify the getCurrentChallengeProgress function to also fetch FindIt and FixIt codes
func getCurrentChallengeProgress(team string) (ChallengeProgressWithCodes, error) {
	// Get regular challenge progress
	challengeStatus, err := getCurrentChallengeStatuses(team)
	if err != nil {
		return ChallengeProgressWithCodes{}, err
	}

	// Fetch FindIt continue code
	continueCodeFindIt, err := getContinueCode(team, "find-it")
	if err != nil {
		logger.Println(fmt.Errorf("failed to fetch FindIt continue code for team '%s': %w", team, err))
		// Don't fail completely if we can't get one code type
		continueCodeFindIt = ""
	}

	// Fetch FixIt continue code
	continueCodeFixIt, err := getContinueCode(team, "fix-it")
	if err != nil {
		logger.Println(fmt.Errorf("failed to fetch FixIt continue code for team '%s': %w", team, err))
		// Don't fail completely if we can't get one code type
		continueCodeFixIt = ""
	}

	return ChallengeProgressWithCodes{
		Challenges:         challengeStatus,
		ContinueCodeFindIt: continueCodeFindIt,
		ContinueCodeFixIt:  continueCodeFixIt,
	}, nil
}

// Helper function to get continue codes
func getContinueCode(team string, codeType string) (string, error) {
	url := fmt.Sprintf("http://juiceshop-%s:3000/rest/continue-code/%s", team, codeType)

	req, err := http.NewRequest("GET", url, bytes.NewBuffer([]byte{}))
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch continue code: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return "", fmt.Errorf("unexpected response status code '%d' from Juice Shop", res.StatusCode)
	}

	var codeResponse struct {
		ContinueCode string `json:"continueCode"`
	}

	err = json.NewDecoder(res.Body).Decode(&codeResponse)
	if err != nil {
		return "", fmt.Errorf("failed to parse JSON from Juice Shop continue code response: %w", err)
	}

	return codeResponse.ContinueCode, nil
}

// Rename the existing function to be more specific
func getCurrentChallengeStatuses(team string) ([]ChallengeStatus, error) {
	url := fmt.Sprintf("http://juiceshop-%s:3000/api/challenges", team)

	req, err := http.NewRequest("GET", url, bytes.NewBuffer([]byte{}))
	if err != nil {
		panic("Failed to create http request")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.New("failed to fetch Challenge Status")
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case 200:
		defer res.Body.Close()

		challengeResponse := ChallengeResponse{}

		err = json.NewDecoder(res.Body).Decode(&challengeResponse)
		if err != nil {
			return nil, errors.New("failed to parse JSON from Juice Shop Challenge Status response")
		}

		challengeStatus := make(ChallengeStatuses, 0)

		for _, challenge := range challengeResponse.Data {
			if challenge.Solved {
				challengeStatus = append(challengeStatus, ChallengeStatus{
					Key:      challenge.Key,
					SolvedAt: challenge.UpdatedAt,
				})
			}
		}

		sort.Stable(challengeStatus)

		return challengeStatus, nil
	default:
		return nil, fmt.Errorf("unexpected response status code '%d' from Juice Shop", res.StatusCode)
	}
}

// Update the workOnProgressUpdates function
func workOnProgressUpdates(progressUpdateJobs <-chan ProgressUpdateJobs, clientset *kubernetes.Clientset) {
	for job := range progressUpdateJobs {
		team := job.Team
		lastChallengeProgress := job.LastChallengeProgress

		// Get deployment to extract existing FindIt and FixIt codes
		deployment, err := clientset.AppsV1().Deployments(os.Getenv("NAMESPACE")).Get(
			context.TODO(),
			fmt.Sprintf("juiceshop-%s", team),
			metav1.GetOptions{},
		)

		// Initialize variables for existing codes
		var lastFindItCode, lastFixItCode string

		// Extract existing FindIt and FixIt codes from deployment annotations
		if err == nil {
			lastFindItCode = deployment.Annotations["multi-juicer.owasp-juice.shop/continueCodeFindIt"]
			lastFixItCode = deployment.Annotations["multi-juicer.owasp-juice.shop/continueCodeFixIt"]
		}

		// Get current progress including FindIt and FixIt codes
		currentProgress, err := getCurrentChallengeProgress(team)
		if err != nil {
			logger.Println(fmt.Errorf("failed to fetch current Challenge Progress for team '%s' from Juice Shop: %w", team, err))
			continue
		}

		// Compare regular challenge progress
		switch CompareChallengeStates(currentProgress.Challenges, lastChallengeProgress) {
		case ApplyCode:
			logger.Printf("Last ContinueCode for team '%s' contains unsolved challenges", team)

			// Apply regular challenge progress
			applyChallengeProgress(team, lastChallengeProgress)

			// Apply FindIt code if available
			if lastFindItCode != "" {
				applyCode(team, lastFindItCode, "find-it")
			}

			// Apply FixIt code if available
			if lastFixItCode != "" {
				applyCode(team, lastFixItCode, "fix-it")
			}

			// Re-fetch the complete progress after applying codes
			currentProgress, err = getCurrentChallengeProgress(team)
			if err != nil {
				logger.Println(fmt.Errorf("failed to re-fetch challenge progress from Juice Shop for team '%s' to reapply it: %w", team, err))
				continue
			}

			PersistProgress(clientset, team, currentProgress.Challenges, currentProgress.ContinueCodeFindIt, currentProgress.ContinueCodeFixIt)

		case UpdateCache:
			PersistProgress(clientset, team, currentProgress.Challenges, currentProgress.ContinueCodeFindIt, currentProgress.ContinueCodeFixIt)

		case NoOp:
			// Even if regular challenges haven't changed, we should update FindIt and FixIt codes if they've changed
			if currentProgress.ContinueCodeFindIt != lastFindItCode || currentProgress.ContinueCodeFixIt != lastFixItCode {
				PersistProgress(clientset, team, currentProgress.Challenges, currentProgress.ContinueCodeFindIt, currentProgress.ContinueCodeFixIt)
			}
		}
	}
}

// Helper function to apply a continue code
func applyCode(team string, code string, codeType string) {
	endpoint := "continue-code"
	if codeType != "" {
		endpoint = fmt.Sprintf("continue-code/%s", codeType)
	}

	url := fmt.Sprintf("http://juiceshop-%s:3000/rest/%s/apply/%s", team, endpoint, code)

	req, err := http.NewRequest("PUT", url, bytes.NewBuffer([]byte{}))
	if err != nil {
		logger.Println(fmt.Errorf("failed to create http request to set the continue code: %w", err))
		return
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Println(fmt.Errorf("failed to set the continue code to juice shop: %w", err))
		return
	}
	defer res.Body.Close()
}

// Update the existing applyChallengeProgress function to use the new applyCode helper
func applyChallengeProgress(team string, challengeProgress []ChallengeStatus) {
	continueCode, err := GenerateContinueCode(challengeProgress)
	if err != nil {
		logger.Println(fmt.Errorf("failed to encode challenge progress into continue code: %w", err))
		return
	}

	applyCode(team, continueCode, "")
}

// ParseContinueCode returns the number of challenges solved by this ContinueCode
func GenerateContinueCode(challenges []ChallengeStatus) (string, error) {
	hd := hashids.NewData()
	hd.Salt = "this is my salt"
	hd.MinLength = 60
	hd.Alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"

	hashIDClient, _ := hashids.NewWithData(hd)

	challengeIds := []int{}

	for _, challenge := range challenges {
		challengeIds = append(challengeIds, challengeIdLookup[challenge.Key])
	}

	continueCode, err := hashIDClient.Encode(challengeIds)

	if err != nil {
		return "", err
	}

	return continueCode, nil
}
