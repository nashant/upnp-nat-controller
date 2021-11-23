import kopf

from kopf import Logger, Meta, Spec, Status, Annotations

from const import KOPF_LB_PARAMS
from utils import get_svc
from loadbalancer import LoadBalancer


@kopf.on.create('services', **KOPF_LB_PARAMS)
@kopf.on.resume('services', **KOPF_LB_PARAMS)
def create_lb(logger: Logger, meta: Meta, spec: Spec, status: Status, annotations: Annotations, **_):
    logger.info(f"Adding LoadBalancer {meta.namespace}/{meta.name}")
    lb = LoadBalancer(
        ip=status["loadBalancer"]["ingress"][0]["ip"],
        annotations=annotations,
        svc=get_svc()
    )
    print(lb.json())


@kopf.on.update('services', **KOPF_LB_PARAMS)
def update_lb(logger: Logger, meta: Meta, old: Spec, new: Status, **_):
    logger.info(f"Updating LoadBalancer {meta.namespace}/{meta.name}")


@kopf.on.delete('services', **KOPF_LB_PARAMS)
def delete_lb(logger: Logger, meta: Meta, spec: Spec, status: Status, **_):
    logger.info(f"Deleting LoadBalancer {meta.namespace}/{meta.name}")

