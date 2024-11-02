package routes

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/juice-shop/multi-juicer/balancer/pkg/bundle"
	"github.com/juice-shop/multi-juicer/balancer/pkg/teamcookie"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type AdminListInstancesResponse struct {
	Instances []AdminListJuiceShopInstance `json:"instances"`
}

type AdminListJuiceShopInstance struct {
	Team        string `json:"team"`
	Ready       bool   `json:"ready"`
	CreatedAt   int64  `json:"createdAt"`
	LastConnect int64  `json:"lastConnect"`
}

func handleAdminListInstances(bundle *bundle.Bundle) http.Handler {
	return http.HandlerFunc(
		func(responseWriter http.ResponseWriter, req *http.Request) {
			team, err := teamcookie.GetTeamFromRequest(bundle, req)
			if err != nil || team != "admin" {
				http.Error(responseWriter, "", http.StatusUnauthorized)
				return
			}

			deployments, err := bundle.ClientSet.AppsV1().Deployments(bundle.RuntimeEnvironment.Namespace).List(req.Context(), metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=juice-shop,app.kubernetes.io/part-of=multi-juicer",
			})
			if err != nil {
				bundle.Log.Printf("Failed to list deployments: %s", err)
				http.Error(responseWriter, "unable to get instances", http.StatusInternalServerError)
				return
			}

			instances := []AdminListJuiceShopInstance{}
			for _, teamDeployment := range deployments.Items {

				lastConnectAnnotation := teamDeployment.Annotations["multi-juicer.owasp-juice.shop/lastRequest"]
				lastConnection := time.UnixMilli(0)

				if lastConnectAnnotation != "" {
					millis, err := strconv.ParseInt(lastConnectAnnotation, 10, 64)
					if err != nil {
						millis = 0
					}
					lastConnection = time.UnixMilli(millis)
				}

				instances = append(instances, AdminListJuiceShopInstance{
					Team:        teamDeployment.Labels["team"],
					Ready:       teamDeployment.Status.ReadyReplicas == 1,
					CreatedAt:   teamDeployment.CreationTimestamp.UnixMilli(),
					LastConnect: lastConnection.UnixMilli(),
				})
			}

			response := AdminListInstancesResponse{
				Instances: instances,
			}

			responseBody, _ := json.Marshal(response)
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.WriteHeader(http.StatusOK)
			responseWriter.Write(responseBody)
		},
	)
}
