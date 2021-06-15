package main

import (
	"log"
	"encoding/json"
	"github.com/gin-gonic/gin"
	corev1		"k8s.io/api/core/v1"
	admissionv1 "k8s.io/api/admission/v1"
)

type JSONPatchEntry struct {
	OP    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

func handleMutate(c *gin.Context) {
	// Deserialize request
	admissionReview := &admissionv1.AdmissionReview{}
	if err := c.Bind(admissionReview); err != nil {
		log.Println("Error deserializing admission review:", err)
		return
	}

	// Default, passthrough response
	admissionResponse := &admissionv1.AdmissionResponse{}
	admissionResponse.UID = admissionReview.Request.UID
	admissionResponse.Allowed = true
	response := &admissionv1.AdmissionReview{}
	response.Response = admissionResponse
	response.SetGroupVersionKind(admissionReview.GroupVersionKind())

	// Filter out unwanted requests
	admissionRequest := admissionReview.Request
	if !(admissionRequest.Kind.Kind == "Pod" && admissionRequest.Operation == admissionv1.Create) {
		log.Println("Admission request with kind", admissionRequest.Kind.Kind, "and operation", admissionRequest.Operation, "skipping")
		c.JSON(200, response)
		return
	}

	// Deserialize pod object
	pod := &corev1.Pod{}
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, pod); err != nil {
		log.Println("Error deserializing pod object from admission review:", err)
		c.JSON(200, response)
		return
	}

	// Check and override command
	containersChanged := false
	if cmds, ok := pod.Annotations["inject/command"]; ok {
		log.Println("Replacing pod command with:", cmds)

		var cmd []string
		if err := json.Unmarshal([]byte(cmds), &cmd); err == nil {
			for i := 0; i < len(pod.Spec.Containers); i++ {
				pod.Spec.Containers[i].Command = cmd
			}
			containersChanged = true
		} else {
			log.Println("Error deserializing command array:", err)
		}
	}

	// Create containersPatch if containers object changed
	var containersPatch *JSONPatchEntry
	if containersChanged {
		containersBytes, err := json.Marshal(pod.Spec.Containers)
		if err == nil {
			containersPatch = &JSONPatchEntry{
				OP:    "replace",
				Path:  "/spec/containers",
				Value: containersBytes,
			}
		} else {
			log.Println("Could not serialize spec.containers:", err)
		}
	}

	// Append non-nil patch entries
	patch := []JSONPatchEntry{}
	if containersPatch != nil {
		patch = append(patch, *containersPatch)
	}

	// Add patch to response if non empty
	if len(patch) > 0 {
		log.Println("Applying", len(patch), "patches in response")
		patchBytes, err := json.Marshal(patch)
		if err == nil {
			patchType := admissionv1.PatchTypeJSONPatch
			admissionResponse.Patch = patchBytes
			admissionResponse.PatchType = &patchType
		} else {
			log.Println("Could not serialize patch:", err)
		}
	}

	// Send back response
	c.JSON(200, response)
}

func main() {
	r := gin.Default()
	r.POST("/mutate", handleMutate)
	r.RunTLS(":8080", "/tls/tls.crt", "/tls/tls.key")
}
