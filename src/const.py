from utils import advertise_tcp, advertise_udp


KOPF_LB_PARAMS = {
    "field": "spec.type",
    "value": "LoadBalancer",
    "when": advertise_tcp or advertise_udp
}
IGD_ARGS = ("crd.nashes.uk", "v1alpha1", "internetgatewaydevices")
IGD_NAME = "igd-device"
IGD_LABELS = {"labels": {"createdBy": "igd-controller"}}
IGD_TIMER = {**IGD_LABELS, "interval": 10, "initial_delay": 60}