package main

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/gin-gonic/gin"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
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

	// Check and inject mounts
	volumesChanged := false
	if mounts, ok := pod.Annotations["inject/mounts"]; ok {
		log.Println("Adding mounts:", mounts)
		var entries []string
		if err := json.Unmarshal([]byte(mounts), &entries); err == nil {
			for _, e := range entries {
				parts := strings.Split(e, ":")
				cmsrc := strings.Split(parts[0], "/")
				if len(parts) != 2 || len(cmsrc) != 2 {
					log.Println("Malformed mount entry, skipping")
					continue
				}

				cname, cpath := cmsrc[0], cmsrc[1]
				dpath := parts[1]

				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
					Name: cname,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: cname,
						},
					},
				})
				for j := 0; j < len(pod.Spec.Containers); j++ {
					container := &pod.Spec.Containers[j]
					container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
						Name:      cname,
						ReadOnly:  true,
						SubPath:   cpath,
						MountPath: dpath,
					})
				}
				containersChanged = true
				volumesChanged = true
			}
		} else {
			log.Println("Error deserializing mounts array:", err)
		}
	}

	initContainersChanged := false
	if tlsSecret, ok := pod.Annotations["inject/certificate"]; ok {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: tlsSecret,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: tlsSecret,
				},
			},
		})
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "certs",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		})
		volumesChanged = true

		for i := 0; i < len(pod.Spec.Containers); i++ {
			container := &pod.Spec.Containers[i]
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      "certs",
				ReadOnly:  false,
				MountPath: "/etc/ssl/certs",
			})
		}
		containersChanged = true

		pod.Spec.InitContainers = append([]v1.Container{
			{
				Name:  "inject-certificate",
				Image: pod.Spec.Containers[0].Image,
				Command: []string{
					"/bin/sh",
					"-c",
					"update-ca-certificates && cp -r /etc/ssl/certs/. /certificates",
				},
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "certs",
						ReadOnly:  false,
						MountPath: "/certificates",
					},
					{
						Name:      tlsSecret,
						ReadOnly:  true,
						SubPath:   "tls.crt",
						MountPath: "/usr/local/share/ca-certificates/" + tlsSecret + ".crt",
					},
				},
			},
		}, pod.Spec.InitContainers...)
		initContainersChanged = true
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

	// Create volumesPatch if volumes object changed
	var volumesPatch *JSONPatchEntry
	if volumesChanged {
		volumesBytes, err := json.Marshal(pod.Spec.Volumes)
		if err == nil {
			volumesPatch = &JSONPatchEntry{
				OP:    "replace",
				Path:  "/spec/volumes",
				Value: volumesBytes,
			}
		} else {
			log.Println("Could not serialize spec.volumes:", err)
		}
	}

	// Create initContainersPatch if initContainers object changed
	var initContainersPatch *JSONPatchEntry
	if initContainersChanged {
		initContainersBytes, err := json.Marshal(pod.Spec.InitContainers)
		if err == nil {
			initContainersPatch = &JSONPatchEntry{
				OP:    "replace",
				Path:  "/spec/initContainers",
				Value: initContainersBytes,
			}
		} else {
			log.Println("Could not serialize spec.initContainers:", err)
		}
	}

	// Append non-nil patch entries
	patches := []JSONPatchEntry{}
	if containersPatch != nil {
		patches = append(patches, *containersPatch)
	}
	if volumesPatch != nil {
		patches = append(patches, *volumesPatch)
	}
	if initContainersPatch != nil {
		patches = append(patches, *initContainersPatch)
	}

	// Add patches to response if non empty
	if len(patches) > 0 {
		log.Println("Applying", len(patches), "patches in response")
		patchBytes, err := json.Marshal(patches)
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
