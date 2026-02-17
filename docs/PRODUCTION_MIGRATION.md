# Production Migration — Step-by-Step

All commands assume you're in `~/work/unicity/sphere-infra/aws/`. Stack is `sphere-zooid-relay` in `me-central-1`.

---

## Prerequisites

- AWS CLI configured for `me-central-1`
- Access to the `sphere-zooid-relay` CloudFormation stack
- Docker + GHCR push access
- `wscat` for verification (`npm i -g wscat`)

---

## Step 1: Build and push images

```bash
cd ~/work/zooid

# 1a. Build the new PostgreSQL relay image
docker build -t ghcr.io/unicitynetwork/unicity-relay:pg-migration .
docker push ghcr.io/unicitynetwork/unicity-relay:pg-migration
```

Create `Dockerfile.migrate` in the repo root:
```dockerfile
FROM golang:1.24 AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY zooid zooid
COPY cmd cmd
RUN CGO_ENABLED=1 GOOS=linux go build -o bin/migrate cmd/migrate/main.go

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /app/bin/migrate /bin/migrate
ENTRYPOINT ["/bin/migrate"]
```

```bash
# 1b. Build the migration tool image
docker build -f Dockerfile.migrate -t ghcr.io/unicitynetwork/unicity-relay-migrate:latest .
docker push ghcr.io/unicitynetwork/unicity-relay-migrate:latest
```

---

## Step 2: Generate a database password

```bash
DB_PASSWORD=$(openssl rand -base64 24)
echo "Save this securely: $DB_PASSWORD"
```

---

## Step 3: Add RDS to CloudFormation (relay stays live on SQLite)

Edit `~/work/unicity/sphere-infra/aws/zooid-relay-cloudformation.yaml`:

**Add parameter** (after `AdminPubkeys`):
```yaml
  DBPassword:
    Type: String
    Description: RDS master password
    NoEcho: true
    MinLength: 16
```

**Add resources** (after `EFSAccessPoint`):
```yaml
  RDSSubnetGroup:
    Type: AWS::RDS::DBSubnetGroup
    Properties:
      DBSubnetGroupDescription: Subnets for Zooid RDS
      SubnetIds:
        - !Ref PublicSubnet1
        - !Ref PublicSubnet2

  RDSSecurityGroup:
    Type: AWS::EC2::SecurityGroup
    Properties:
      GroupDescription: Allow PostgreSQL from ECS tasks
      VpcId: !Ref VPC
      SecurityGroupIngress:
        - IpProtocol: tcp
          FromPort: 5432
          ToPort: 5432
          SourceSecurityGroupId: !Ref ECSSecurityGroup
      Tags:
        - Key: Name
          Value: !Sub ${AWS::StackName}-rds-sg

  RDSInstance:
    Type: AWS::RDS::DBInstance
    Properties:
      DBInstanceIdentifier: !Sub ${AWS::StackName}-db
      DBInstanceClass: db.t3.micro
      Engine: postgres
      EngineVersion: '16'
      DBName: zooid
      MasterUsername: zooid
      MasterUserPassword: !Ref DBPassword
      AllocatedStorage: 20
      StorageType: gp3
      StorageEncrypted: true
      VPCSecurityGroups:
        - !Ref RDSSecurityGroup
      DBSubnetGroupName: !Ref RDSSubnetGroup
      PubliclyAccessible: false
      MultiAZ: false
      BackupRetentionPeriod: 7
      DeletionProtection: true
      Tags:
        - Key: Name
          Value: !Sub ${AWS::StackName}-rds
```

**Add output** (after existing outputs):
```yaml
  RDSEndpoint:
    Description: RDS PostgreSQL endpoint
    Value: !GetAtt RDSInstance.Endpoint.Address
    Export:
      Name: !Sub ${AWS::StackName}-rds-endpoint
```

**Do NOT change the task definition yet** — the relay stays on SQLite during this step.

Deploy:
```bash
cd ~/work/unicity/sphere-infra/aws

PARAMS_FILE=$(mktemp)
cat > "$PARAMS_FILE" << EOF
[
  {"ParameterKey": "RelaySecretKey", "UsePreviousValue": true},
  {"ParameterKey": "AdminPubkey", "UsePreviousValue": true},
  {"ParameterKey": "AdminPubkeys", "UsePreviousValue": true},
  {"ParameterKey": "DBPassword", "ParameterValue": "$DB_PASSWORD"}
]
EOF

aws cloudformation update-stack \
  --stack-name sphere-zooid-relay \
  --template-body file://zooid-relay-cloudformation.yaml \
  --parameters file://"$PARAMS_FILE" \
  --capabilities CAPABILITY_IAM \
  --region me-central-1

rm -f "$PARAMS_FILE"

# Wait ~10 min for RDS to create
aws cloudformation wait stack-update-complete \
  --stack-name sphere-zooid-relay \
  --region me-central-1
```

Verify RDS is ready:
```bash
RDS_ENDPOINT=$(aws cloudformation describe-stacks \
  --stack-name sphere-zooid-relay \
  --query 'Stacks[0].Outputs[?OutputKey==`RDSEndpoint`].OutputValue' \
  --output text --region me-central-1)
echo "RDS endpoint: $RDS_ENDPOINT"
```

---

## Step 4: Register a migration task definition

This one-shot task needs both EFS (to read SQLite) and RDS access.

```bash
# Get existing resource IDs
EFS_FS_ID=$(aws cloudformation describe-stacks \
  --stack-name sphere-zooid-relay \
  --query 'Stacks[0].Outputs[?OutputKey==`EFSFileSystemId`].OutputValue' \
  --output text --region me-central-1)

EFS_AP_ID=$(aws cloudformation describe-stack-resources \
  --stack-name sphere-zooid-relay \
  --logical-resource-id EFSAccessPoint \
  --query 'StackResources[0].PhysicalResourceId' \
  --output text --region me-central-1)

EXECUTION_ROLE=$(aws cloudformation describe-stack-resources \
  --stack-name sphere-zooid-relay \
  --logical-resource-id ECSTaskExecutionRole \
  --query 'StackResources[0].PhysicalResourceId' \
  --output text --region me-central-1)

TASK_ROLE=$(aws cloudformation describe-stack-resources \
  --stack-name sphere-zooid-relay \
  --logical-resource-id ECSTaskRole \
  --query 'StackResources[0].PhysicalResourceId' \
  --output text --region me-central-1)

LOG_GROUP=$(aws cloudformation describe-stack-resources \
  --stack-name sphere-zooid-relay \
  --logical-resource-id CloudWatchLogGroup \
  --query 'StackResources[0].PhysicalResourceId' \
  --output text --region me-central-1)

DATABASE_URL="postgres://zooid:${DB_PASSWORD}@${RDS_ENDPOINT}:5432/zooid?sslmode=require"
```

```bash
cat > /tmp/migrate-task-def.json << EOF
{
  "family": "sphere-zooid-relay-migrate",
  "networkMode": "awsvpc",
  "requiresCompatibilities": ["FARGATE"],
  "cpu": "512",
  "memory": "1024",
  "executionRoleArn": "${EXECUTION_ROLE}",
  "taskRoleArn": "${TASK_ROLE}",
  "volumes": [{
    "name": "zooid-data",
    "efsVolumeConfiguration": {
      "fileSystemId": "${EFS_FS_ID}",
      "transitEncryption": "ENABLED",
      "authorizationConfig": {
        "accessPointId": "${EFS_AP_ID}",
        "iam": "ENABLED"
      }
    }
  }],
  "containerDefinitions": [{
    "name": "migrate",
    "image": "ghcr.io/unicitynetwork/unicity-relay-migrate:latest",
    "essential": true,
    "environment": [
      {"name": "SQLITE_PATH", "value": "/app/data/db"},
      {"name": "DATABASE_URL", "value": "${DATABASE_URL}"}
    ],
    "mountPoints": [{
      "sourceVolume": "zooid-data",
      "containerPath": "/app/data",
      "readOnly": true
    }],
    "logConfiguration": {
      "logDriver": "awslogs",
      "options": {
        "awslogs-group": "${LOG_GROUP}",
        "awslogs-region": "me-central-1",
        "awslogs-stream-prefix": "migrate"
      }
    }
  }]
}
EOF

aws ecs register-task-definition \
  --cli-input-json file:///tmp/migrate-task-def.json \
  --region me-central-1
```

---

## Step 5: Cutover (~5 min downtime)

### 5a. Stop the relay (prevents writes during migration)

```bash
aws ecs update-service \
  --cluster sphere-zooid-relay-cluster \
  --service sphere-zooid-relay-zooid-relay \
  --desired-count 0 \
  --region me-central-1

# Wait for task to drain
aws ecs wait services-stable \
  --cluster sphere-zooid-relay-cluster \
  --services sphere-zooid-relay-zooid-relay \
  --region me-central-1
```

### 5b. Run the migration

```bash
SUBNET1=$(aws cloudformation describe-stack-resources \
  --stack-name sphere-zooid-relay \
  --logical-resource-id PublicSubnet1 \
  --query 'StackResources[0].PhysicalResourceId' \
  --output text --region me-central-1)

SUBNET2=$(aws cloudformation describe-stack-resources \
  --stack-name sphere-zooid-relay \
  --logical-resource-id PublicSubnet2 \
  --query 'StackResources[0].PhysicalResourceId' \
  --output text --region me-central-1)

ECS_SG=$(aws cloudformation describe-stack-resources \
  --stack-name sphere-zooid-relay \
  --logical-resource-id ECSSecurityGroup \
  --query 'StackResources[0].PhysicalResourceId' \
  --output text --region me-central-1)

aws ecs run-task \
  --cluster sphere-zooid-relay-cluster \
  --task-definition sphere-zooid-relay-migrate \
  --launch-type FARGATE \
  --platform-version 1.4.0 \
  --network-configuration "awsvpcConfiguration={subnets=[$SUBNET1,$SUBNET2],securityGroups=[$ECS_SG],assignPublicIp=ENABLED}" \
  --region me-central-1
```

### 5c. Watch the migration logs

```bash
# Wait a minute for the task to start, then tail logs
aws logs tail "$LOG_GROUP" \
  --prefix migrate \
  --follow \
  --region me-central-1
```

Expected output:
```
Connected to both databases
Found tables: [kv sphere_relay__event_tags sphere_relay__events]
Migrating sphere_relay__events: N rows
Migrating sphere_relay__event_tags: N rows
Migrating kv: N rows
  kv: source=X dest=X [OK]
  sphere_relay__event_tags: source=X dest=X [OK]
  sphere_relay__events: source=X dest=X [OK]
Migration completed successfully!
```

**ALL counts must show `[OK]`. If ANY show `MISMATCH` — STOP. Do not proceed. Jump to [Rollback](#rollback).**

### 5d. Update CloudFormation for the PostgreSQL relay

In `zooid-relay-cloudformation.yaml`, update the task definition:

1. Change `ContainerImage` default to `ghcr.io/unicitynetwork/unicity-relay:pg-migration`
2. Add `DATABASE_URL` env var to `ContainerDefinitions.Environment`:
   ```yaml
   - Name: DATABASE_URL
     Value: !Sub postgres://zooid:${DBPassword}@${RDSInstance.Endpoint.Address}:5432/zooid?sslmode=require
   ```
3. Remove `DATA` env var
4. Remove `MountPoints` block from `ContainerDefinitions`
5. Remove `Volumes` block from `ZooidRelayTaskDefinition`
6. Remove `EFSMountTarget1` and `EFSMountTarget2` from `ZooidRelayService.DependsOn`

Deploy:
```bash
PARAMS_FILE=$(mktemp)
cat > "$PARAMS_FILE" << EOF
[
  {"ParameterKey": "RelaySecretKey", "UsePreviousValue": true},
  {"ParameterKey": "AdminPubkey", "UsePreviousValue": true},
  {"ParameterKey": "AdminPubkeys", "UsePreviousValue": true},
  {"ParameterKey": "DBPassword", "UsePreviousValue": true},
  {"ParameterKey": "ContainerImage", "ParameterValue": "ghcr.io/unicitynetwork/unicity-relay:pg-migration"}
]
EOF

aws cloudformation update-stack \
  --stack-name sphere-zooid-relay \
  --template-body file://zooid-relay-cloudformation.yaml \
  --parameters file://"$PARAMS_FILE" \
  --capabilities CAPABILITY_IAM \
  --region me-central-1

rm -f "$PARAMS_FILE"

aws cloudformation wait stack-update-complete \
  --stack-name sphere-zooid-relay \
  --region me-central-1
```

### 5e. Start the relay

```bash
aws ecs update-service \
  --cluster sphere-zooid-relay-cluster \
  --service sphere-zooid-relay-zooid-relay \
  --desired-count 1 \
  --region me-central-1
```

### 5f. Verify it's working

```bash
# Check task is running
aws ecs describe-services \
  --cluster sphere-zooid-relay-cluster \
  --services sphere-zooid-relay-zooid-relay \
  --query 'services[0].{running: runningCount, desired: desiredCount}' \
  --region me-central-1

# Check logs for startup
aws logs tail "$LOG_GROUP" \
  --prefix zooid-relay \
  --follow \
  --region me-central-1

# Test WebSocket
wscat -c wss://sphere-relay.unicity.network
# Type: ["REQ","test",{"limit":1}]
# Should receive an event back
```

---

## Rollback

SQLite data on EFS is **untouched** (migration opens it read-only). To roll back at any point:

```bash
# 1. Revert the CloudFormation template to the old version
#    (SQLite image + EFS mount + DATA env var)

# 2. Deploy with the old ContainerImage
PARAMS_FILE=$(mktemp)
cat > "$PARAMS_FILE" << EOF
[
  {"ParameterKey": "RelaySecretKey", "UsePreviousValue": true},
  {"ParameterKey": "AdminPubkey", "UsePreviousValue": true},
  {"ParameterKey": "AdminPubkeys", "UsePreviousValue": true},
  {"ParameterKey": "DBPassword", "UsePreviousValue": true},
  {"ParameterKey": "ContainerImage", "ParameterValue": "ghcr.io/unicitynetwork/unicity-relay:sha-44d3ff3"}
]
EOF

aws cloudformation update-stack \
  --stack-name sphere-zooid-relay \
  --template-body file://zooid-relay-cloudformation.yaml \
  --parameters file://"$PARAMS_FILE" \
  --capabilities CAPABILITY_IAM \
  --region me-central-1

rm -f "$PARAMS_FILE"

# 3. Set desired count back to 1
aws ecs update-service \
  --cluster sphere-zooid-relay-cluster \
  --service sphere-zooid-relay-zooid-relay \
  --desired-count 1 \
  --region me-central-1
```

---

## Step 6: Post-migration cleanup (after 1 week of confidence)

Remove from CloudFormation template:
- `EFSFileSystem`, `EFSMountTarget1`, `EFSMountTarget2`, `EFSAccessPoint`, `EFSSecurityGroup` resources
- `EFSFileSystemId` output
- `EFSAccess` policy from `ECSTaskRole`

Other cleanup:
- Deregister the migration task definition
- Delete `Dockerfile.migrate`
- Retag the relay image as the new default
