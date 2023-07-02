import kopf
import uuid

from kopf import Logger, Memo
from kubernetes import config, client
from kubernetes.client import CustomObjectsApi
from kubernetes.client.exceptions import ApiException
from pydantic import BaseModel
from typing import Optional
from upnpy.ssdp.SSDPDevice import SSDPDevice
from upnpy.exceptions import SOAPError

from utils import get_device
from const import IGD_ARGS, IGD_NAME, IGD_LABELS, IGD_TIMER

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


def create_igd(api, name) -> None:
    body = {
        "apiVersion": "crd.nashes.uk/v1alpha1",
        "kind": "InternetGatewayDevice",
        "metadata": {
            "name": name,
            "labels": {
                "createdBy": "igd-controller"
            }
        }
    }
    api.create_cluster_custom_object(*IGD_ARGS, body)


@kopf.on.startup()
def reset_igd(memo: Memo, **_):
    api = get_kube_api()

    igds = api.list_cluster_custom_object(*IGD_ARGS)
    for igd in igds.get("items", []):
        api.delete_cluster_custom_object(*IGD_ARGS, igd["metadata"]["name"])

    memo["igd"] = IGD(get_device())
    memo["igd_name"] = f"{IGD_NAME}-{uuid.uuid1().hex}"
    create_igd(api, memo["igd_name"])


def run_timers(memo: Memo, **_):
    return memo.run_timers


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def ip(logger: Logger, memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return igd.dev.host


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def friendlyName(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return igd.dev.friendly_name


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def externalIpAddress(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    try:
        return igd.GetExternalIPAddress().get("NewExternalIPAddress")
    except urllib.error.URLError:
        memo["igd"] = IGD(get_device())


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def connectionStatus(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return igd.GetStatusInfo().get("NewConnectionStatus")


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def uptime(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetStatusInfo().get("NewUptime"))


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def totalBytesSent(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalBytesSent().get("NewTotalBytesSent"))


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def totalBytesReceived(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalBytesReceived().get("NewTotalBytesReceived"))


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def totalPacketsSent(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalPacketsSent().get("NewTotalPacketsSent"))


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def totalPacketsReceived(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    return int(igd.GetTotalPacketsReceived().get("NewTotalPacketsReceived"))


@kopf.on.create('internetgatewaydevices', **IGD_LABELS)
@kopf.on.timer('internetgatewaydevices', **IGD_TIMER)
def portMappings(memo: Memo, **_):
    igd: IGD = memo.get("igd", None)
    port_mappings = []
    i = 0
    while True:
        try:
            pm = igd.GetGenericPortMappingEntry(NewPortMappingIndex=i)
            port_mappings.append(PortMapping.parse(pm).dict())
            i += 1
        except SOAPError as e:
            if e.error == 713:
                break
            raise(e)
    return port_mappings


@kopf.on.field('internetgatewaydevices', field='status.portMappings', **IGD_LABELS)
def ensurePortMappings(logger: Logger, memo: Memo, **_):
    if 'lb' in memo:
        logger.info(f"Ensuring loadbalancer ports are advertised")
        memo.lb.advertise(logger)
