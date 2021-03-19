#!/bin/sh
IMAGE=$1
kubectl run csi-sanity --image=$IMAGE --overrides='
{
  	"apiVersion": "v1",
	"spec": {
		"containers": [
			{
			"name": "csi-sanity",
			"image": "'$IMAGE'",
			"stdin": true,
			"stdinOnce": true,
			"tty": true,
			"command": ["/app/csi-sanity/csi-sanity"],
			"args": ["--ginkgo.v", "--csi.endpoint=/node.sock", "--csi.controllerendpoint=/controller.sock", "--csi.mountdir=/dev/mnt", "--csi.stagingdir=/dev/stg"],
			"volumeMounts": [{
				"name": "controller",
				"mountPath": "/controller.sock"
			},
			{
				"name": "node",
				"mountPath": "/node.sock"
			}]
			}
		],
		"volumes": [{
			"name":"controller",
			"hostPath":{
				"path": "/var/run/csi/controller-csi.sock"
			}
		},
		{
			"name":"node",
			"hostPath":{
				"path": "/var/run/csi/node-csi.sock"
			}
		}]
	}
}
' --rm -ti --attach --restart=Never