import SwiftUI

/// Manifests shown in the FAQ. They mirror examples/faq-*.yaml byte for byte; a
/// test enforces that, and a Go test checks the files against the translator.
enum FAQExamples {
    static let docker = #"""
        # Default executor: no executor label means Docker. Apply with:
        #   kubectl apply -f faq-docker.yaml
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: faq-docker
          namespace: maccluster-test
        spec:
          replicas: 1
          selector:
            matchLabels:
              app: faq-docker
          template:
            metadata:
              labels:
                app: faq-docker
                maccluster.example.com/managed: "true"
            spec:
              automountServiceAccountToken: false
              nodeSelector:
                maccluster.example.com/host: maccluster-node
              tolerations:
                - key: maccluster.example.com/prototype
                  value: "true"
                  effect: NoSchedule
              containers:
                - name: web
                  image: nginx:alpine
                  ports:
                    - containerPort: 80
                  readinessProbe:
                    httpGet:
                      path: /
                      port: 80
        """#
    static let dockerVolumes = #"""
        # Docker Pod with volumes: a writable emptyDir, a memory emptyDir with a size limit, a ConfigMap and a Secret as files,
        # and the Pod's own name as a file. Apply with:
        #   kubectl apply -f faq-docker-volumes.yaml
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: faq-volumes-config
          namespace: maccluster-test
        data:
          app.conf: |
            mode = demo
        ---
        apiVersion: v1
        kind: Secret
        metadata:
          name: faq-volumes-secret
          namespace: maccluster-test
        stringData:
          token: change-me
        ---
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: faq-docker-volumes
          namespace: maccluster-test
        spec:
          replicas: 1
          selector:
            matchLabels:
              app: faq-docker-volumes
          template:
            metadata:
              labels:
                app: faq-docker-volumes
                maccluster.example.com/managed: "true"
            spec:
              automountServiceAccountToken: false
              nodeSelector:
                maccluster.example.com/host: maccluster-node
              tolerations:
                - key: maccluster.example.com/prototype
                  value: "true"
                  effect: NoSchedule
              containers:
                - name: web
                  image: nginx:alpine
                  ports:
                    - containerPort: 80
                  volumeMounts:
                    - name: data
                      mountPath: /data
                    - name: scratch
                      mountPath: /scratch
                    - name: config
                      mountPath: /etc/app
                      readOnly: true
                    - name: secret
                      mountPath: /etc/secret
                      readOnly: true
                    - name: me
                      mountPath: /etc/me
              volumes:
                - name: data
                  emptyDir: {}
                - name: scratch
                  emptyDir:
                    medium: Memory
                    sizeLimit: 16Mi
                - name: config
                  configMap:
                    name: faq-volumes-config
                - name: secret
                  secret:
                    secretName: faq-volumes-secret
                    defaultMode: 0400
                - name: me
                  downwardAPI:
                    items:
                      - path: name
                        fieldRef:
                          fieldPath: metadata.name
        """#
    static let native = #"""
        # Native executor with an absolute executable path. Apply with:
        #   kubectl apply -f faq-native.yaml
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: faq-native
          namespace: maccluster-test
        spec:
          replicas: 1
          selector:
            matchLabels:
              app: faq-native
          template:
            metadata:
              labels:
                app: faq-native
                maccluster.example.com/managed: "true"
                maccluster.example.com/executor: native
            spec:
              automountServiceAccountToken: false
              nodeSelector:
                maccluster.example.com/host: maccluster-node
              tolerations:
                - key: maccluster.example.com/prototype
                  value: "true"
                  effect: NoSchedule
              containers:
                - name: http
                  image: native/python-http:local
                  command: [/usr/bin/python3]
                  args: [-m, http.server, "$(MACCLUSTER_PORT)", --bind, "127.0.0.1"]
                  ports:
                    - containerPort: 8000
                  readinessProbe:
                    httpGet:
                      path: /
                      port: 8000
        """#
    static let nativeEnvironment = #"""
        # Native executor that runs inside a registered execution environment.
        # Register the environment first (App: toolbar "執行環境" > 新增, or CLI):
        #   maccluster-agent env add --name my-env --root /path/to/venv
        # Then apply with:
        #   kubectl apply -f faq-native-environment.yaml
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: faq-native-env
          namespace: maccluster-test
        spec:
          replicas: 1
          selector:
            matchLabels:
              app: faq-native-env
          template:
            metadata:
              labels:
                app: faq-native-env
                maccluster.example.com/managed: "true"
                maccluster.example.com/executor: native
                maccluster.example.com/environment: my-env
            spec:
              automountServiceAccountToken: false
              nodeSelector:
                maccluster.example.com/host: maccluster-node
                env.maccluster.example.com/my-env: "true"
              tolerations:
                - key: maccluster.example.com/prototype
                  value: "true"
                  effect: NoSchedule
              containers:
                - name: http
                  image: native/my-env:local
                  command: [python3]
                  args: [-m, http.server, "$(MACCLUSTER_PORT)", --bind, "127.0.0.1"]
                  ports:
                    - containerPort: 8000
                  readinessProbe:
                    httpGet:
                      path: /
                      port: 8000
        """#
    static let nativeFixedPorts = #"""
        # Native workload that binds its own fixed ports (see docs/fixed-ports.md). Apply with:
        #   kubectl apply -f faq-native-fixed-ports.yaml
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: vllm-fixed
          namespace: maccluster-test
        spec:
          replicas: 1
          strategy:
            type: Recreate
          selector:
            matchLabels:
              app: vllm-fixed
          template:
            metadata:
              labels:
                app: vllm-fixed
                maccluster.example.com/managed: "true"
                maccluster.example.com/executor: native
                maccluster.example.com/environment: vllm-metal
                # Several ports are separated by "_" (label values cannot contain commas).
                maccluster.example.com/fixed-ports: "8100_9100"
            spec:
              automountServiceAccountToken: false
              nodeSelector:
                maccluster.example.com/host: maccluster-node
                env.maccluster.example.com/vllm-metal: "true"
              tolerations:
                - key: maccluster.example.com/prototype
                  value: "true"
                  effect: NoSchedule
              containers:
                - name: vllm
                  image: native/vllm-metal:local
                  command: [vllm]
                  args: [serve, /path/to/model, --host, "0.0.0.0", --port, "8100"]
                  ports:
                    - containerPort: 8100
                    - containerPort: 9100
                  readinessProbe:
                    httpGet:
                      path: /health
                      port: 8100
        """#
    /// The examples target the prototype test node; show the real node when known.
    static let placeholderNode = "maccluster-node"
}
