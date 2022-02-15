import kopf

from kopf import Logger, Meta, Spec, Status, Annotations

from const import KOPF_LB_PARAMS
from utils import get_svc
from loadbalancer import LoadBalancer


def get_ip(d: dict, status: Status):
    try:
        return d["status"]["loadBalancer"]["ingress"][0]["ip"]
    except (KeyError, IndexError):
        return status["loadBalancer"]["ingress"][0]["ip"]


@kopf.on.create('services', **KOPF_LB_PARAMS)
@kopf.on.resume('services', **KOPF_LB_PARAMS)
def create_lb(logger: Logger, meta: Meta, spec: Spec, status: Status, annotations: Annotations, **_):
    logger.info(f"Adding LoadBalancer {meta.namespace}/{meta.name}")
    lb = LoadBalancer(
        ip=status["loadBalancer"]["ingress"][0]["ip"],
        annotations=annotations,
        svc=get_svc()
    )
    lb.advertise(logger)


@kopf.on.update('services', **KOPF_LB_PARAMS)
def update_lb(logger: Logger, meta: Meta, old: Spec, new: Spec, annotations: Annotations, status: Status, **_):
    logger.info(f"Updating LoadBalancer {meta.namespace}/{meta.name}")
    old_lb = LoadBalancer(
        ip=get_ip(old, status),
        annotations=old.get("spec", {}).get("annotations", annotations),
        svc=get_svc()
    )
    new_lb = LoadBalancer(
        ip=get_ip(new, status),
        annotations=new.get("spec", {}).get("annotations", annotations),
        svc=get_svc()
    )
    old_lb.unadvertise(logger)
    new_lb.advertise(logger)



@kopf.on.delete('services', **KOPF_LB_PARAMS)
def delete_lb(logger: Logger, meta: Meta, spec: Spec, annotations: Annotations, status: Status, **_):
    logger.info(f"Deleting LoadBalancer {meta.namespace}/{meta.name}")
    lb = LoadBalancer(
        ip=status["loadBalancer"]["ingress"][0]["ip"],
        annotations=annotations,
        svc=get_svc()
    )
    lb.unadvertise(logger)
