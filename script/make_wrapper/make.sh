#!/bin/bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

REGISTRY_HOST=registry_integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

function check {
    if [ ${?} = 0 ] ; then
        echo "Completed: ${1}"
    else
        echo "Failed: ${1}"
        exit 1
    fi
}

if [ "${1}" == "" ]; then
    echo "No make command provided"
    exit 1
fi

# NOTE: Specify build args via ${DOCKER_BUILD_ARGS}
echo ${DOCKER_BUILD_ARGS}

COMMAND="${1}"
CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
cd "${CONTEXT}"

TARGETS=
INTEGRATION=false
for T in ${@} ; do
    if [ "${T}" == "integration" ] ; then
        INTEGRATION=true
    else
        TARGETS="${TARGETS} ${T}"
    fi
done

FAIL=false
if [ "${INTEGRATION}" == "true" ] ; then
    # See also: https://docs.docker.com/registry/deploying/
    AUTH_DIR=$(mktemp -d)
    mkdir "${AUTH_DIR}/auth" "${AUTH_DIR}/certs"
    check "Preparing temp dir"

    openssl req -subj "/C=JP/ST=Remote/L=Snapshotter/O=TestEnv/OU=Integration/CN=${REGISTRY_HOST}" \
            -newkey rsa:2048 -nodes -keyout "${AUTH_DIR}/certs/domain.key" \
            -x509 -days 365 -out "${AUTH_DIR}/certs/domain.crt"
    check "Preparing self-signed certs"
    
    docker run --entrypoint htpasswd registry:2 -Bbn "${DUMMYUSER}" "${DUMMYPASS}" > "${AUTH_DIR}/auth/htpasswd"
    check "Preparing authentication information"
    
    DOCKER_COMPOSE_YAML=$(mktemp)
    "${CONTEXT}"/docker-compose-integration.yml.sh "${CONTEXT}/../../" "${AUTH_DIR}" > "${DOCKER_COMPOSE_YAML}"
    check "Preparing docker-compose.yml"

    if ! ( docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS} testenv_integration && \
               docker-compose -f "${DOCKER_COMPOSE_YAML}" up --exit-code-from testenv_integration ) ; then
        FAIL=true
    fi
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down
    rm "${DOCKER_COMPOSE_YAML}"
    rm -rf "${AUTH_DIR}"
fi

if [ "$TARGETS" != "" ] ; then
    docker build ${DOCKER_BUILD_ARGS} -f ./Dockerfile.minimal -t testenv:mini .
    check "Building mini environment"

    if ! docker run --rm --privileged --device /dev/fuse \
         --tmpfs /tmp:exec,mode=777 \
         -w /go/src/github.com/ktock/remote-snapshotter \
         -v "${CONTEXT}/../../:/go/src/github.com/ktock/remote-snapshotter" \
         testenv:mini make $TARGETS ; then
        FAIL=true
    fi
fi

if [ "${FAIL}" == "true" ] ; then
    echo "Some targets failed."
    exit 1
fi

echo "Succeeded all."
exit 0
