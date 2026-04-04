resource "aws_lambda_function" "lambda" {
  for_each = local.lambdas

  function_name    = "${local.name}-${each.key}"
  description      = each.value.description
  role             = aws_iam_role.lambda[each.key].arn
  filename         = data.archive_file.lambda[each.key].output_path
  source_code_hash = data.archive_file.lambda[each.key].output_base64sha256
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  timeout          = 30

  dynamic "environment" {
    for_each = length(each.value.env_vars) > 0 ? [1] : []

    content {
      variables = each.value.env_vars
    }
  }
}

resource "aws_lambda_permission" "scheduler" {
  for_each = local.schedules

  function_name = aws_lambda_function.lambda["scheduler"].function_name
  statement_id  = "AllowEventBridgeInvoke-${each.key}"
  action        = "lambda:InvokeFunction"
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.scheduler[each.key].arn
}

# Security is handled by Ed25519 signature verification in application code,
# which is the approach Discord's own docs prescribe.
resource "aws_lambda_function_url" "receiver" {
  function_name      = aws_lambda_function.lambda["receiver"].function_name
  authorization_type = "NONE"
}

resource "aws_lambda_event_source_mapping" "dispatcher" {
  function_name     = aws_lambda_function.lambda["dispatcher"].arn
  event_source_arn  = aws_dynamodb_table.reminders.stream_arn
  starting_position = "LATEST"

  filter_criteria {
    filter {
      # Only invoke on TTL-triggered removes, not manual deletes.
      pattern = jsonencode({ userIdentity = { type = ["Service"], principalId = ["dynamodb.amazonaws.com"] } })
    }
  }
}

