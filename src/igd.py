import kopf

from kopf import Memo, Meta, Spec
from kubernetes import config, client
from kubernetes.client import CustomObjectsApi
from kubernetes.client.exceptions import ApiException
from pydantic import BaseModel
from typing import Optional
from upnpy.ssdp.SSDPDevice import SSDPDevice
from upnpy.exceptions import SOAPError

from utils import get_device
from const import IGD_ARGS, IGD_NAME

class PortMapping(BaseModel):
    remoteHost: Optional[str]
    externalPort: int
    protocol: str
    internalPort: int
    internalClient: str
    enabled: bool
    portMappingDescription: str
    leaseDuration: int

    @classmethod
    def parse(cls, d: dict):
        mapped = {cls.fix_key(k): v for k, v in d.items()}
        return cls(**mapped)

    @classmethod
    def fix_key(cls, k: str):
        k = k.lstrip("New")
        return f"{k[0].lower()}{k[1:]}"


class IGD:
    def __init__(self, dev: SSDPDevice):
        self.dev = dev
        for svc in dev.get_services():
            svc: SSDPDevice.Service = svc
            for action in svc.get_actions():
                action: SSDPDevice.Service.Action = action
                self.__setattr__(action.name, action)


def get_kube_api() -> CustomObjectsApi:
    config.load_incluster_config()
    return client.CustomObjectsApi()


def get_or_create_igd() -> None:
    api = get_kube_api()
    try:
        api.get_cluster_custom_object(*IGD_ARGS, IGD_NAME)
    except ApiException as e:
        if e.status != 404:
            raise
        body = {
            "apiVersion": "crd.nashes.uk/v1alpha1",
            "kind": "InternetGatewayDevice",
            "metadata": {
                "name": IGD_NAME,
                "labels": {
                    "createdBy": "igd-controller"
                }
            }
        }
        api.create_cluster_custom_object(*IGD_ARGS, body)


@kopf.on.startup()
def set_igd(memo: Memo, **_):
    memo["igd"] = IGD(get_device())
    get_or_create_igd()


@kopf.on.cleanup()
def delete_igd(**_):
    api = get_kube_api()
    api.delete_cluster_custom_object(*IGD_ARGS, IGD_NAME)


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'igd-controller'})
def ip(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return igd.dev.host


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def externalIPAddress(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return igd.GetExternalIPAddress().get("NewExternalIPAddress")


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def friendlyName(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    igd.dev.friendly_name


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def connectionStatus(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return igd.GetStatusInfo().get("NewConnectionStatus")


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def uptime(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetStatusInfo().get("NewUptime"))


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def totalBytesSent(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalBytesSent().get("NewTotalBytesSent"))


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def totalBytesReceived(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalBytesReceived().get("NewTotalBytesReceived"))


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def totalPacketsSent(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalPacketsSent().get("NewTotalPacketsSent"))


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def totalPacketsReceived(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalPacketsReceived().get("NewTotalPacketsReceived"))


@kopf.timer('internetgatewaydevices', interval=10, initial_delay=60,
           labels={'createdBy': 'upnp-nat-controller'})
def portMappings(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    port_mappings = []
    pm_index = 0
    while True:
        try:
            pm = igd.GetGenericPortMappingEntry(NewPortMappingIndex=pm_index)
            port_mappings.append(PortMapping.parse(pm).dict())
            i += 1
        except SOAPError as e:
            if e.error == 713:
                break
            raise(e)
    return port_mappings
