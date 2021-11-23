from utils import advertise_tcp, advertise_udp


KOPF_LB_PARAMS = {
    "field": "spec.type",
    "value": "LoadBalancer",
    "when": advertise_tcp or advertise_udp
}