package kube

// ObjectMeta holds the metadata fields the service reads.
type ObjectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Generation      int64             `json:"generation,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	OwnerReferences []OwnerReference  `json:"ownerReferences,omitempty"`
}

// OwnerReference identifies the controller that owns an object.
type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Condition is a standard status condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Application is the part of an Argo CD Application the service reads.
type Application struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		Resources []ApplicationResource `json:"resources"`
	} `json:"status"`
}

// ApplicationResource is one entry of an Application's status.resources.
type ApplicationResource struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ApplicationList is a list of Applications.
type ApplicationList struct {
	Items []Application `json:"items"`
}

// ConfigMap is a core/v1 ConfigMap.
type ConfigMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Data       map[string]string `json:"data,omitempty"`
}

// TokenReview is an authentication.k8s.io/v1 TokenReview.
type TokenReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Token     string   `json:"token"`
		Audiences []string `json:"audiences,omitempty"`
	} `json:"spec"`
	Status struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			Username string `json:"username"`
		} `json:"user"`
		Audiences []string `json:"audiences,omitempty"`
		Error     string   `json:"error,omitempty"`
	} `json:"status"`
}
