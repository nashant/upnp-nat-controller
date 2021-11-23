from pydantic import BaseModel
from typing import List
from upnpy.ssdp.SSDPDevice import SSDPDevice
from kopf import Annotations


def proto_enabled(proto: str, annotations: Annotations):
    try:
        return annotations[f"{proto}.advertise.upnp/enabled"] == "true"
    except KeyError:
        return False


def get_ports(proto: str, annotations: Annotations) -> List[int]:
    if not proto_enabled(proto):
        return None
    try:
        return annotations[f"{proto}.advertise.upnp/ports"].split(",")
    except KeyError:
        return []


class LoadBalancer(BaseModel):
    ip: str
    svc: SSDPDevice.Service
    annotations: Annotations
    tcp_whitelist: List[int]
    udp_whitelist: List[int]

    class Config:
        arbitrary_types_allowed = True

    def __init__(self, **data):
        data["tcp_whitelist"] = get_ports("tcp", data["annotations"])
        data["udp_whitelist"] = get_ports("udp", data["annotations"])
        super().__init__(**data)
