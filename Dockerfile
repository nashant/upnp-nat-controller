FROM python:3.9-slim-bullseye

EXPOSE 8080/tcp

ADD src /app

RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y vim \
 && pip install --upgrade pip \
 && pip install -r /app/requirements.txt \
 && useradd -mb /tmp -s /bin/bash -u 1000 kopf \
 && chown -R kopf /app
USER kopf
WORKDIR /app
ENTRYPOINT [ "kopf", "run", "--all-namespaces", "./controller.py"]