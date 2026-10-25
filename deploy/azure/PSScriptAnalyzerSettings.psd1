# Invoke-ScriptAnalyzer -Path deploy/azure/deploy.ps1 -Settings deploy/azure/PSScriptAnalyzerSettings.psd1
@{
    Severity     = @('Error', 'Warning', 'Information')
    ExcludeRules = @(
        # an interactive deploy script: its output is for the person running it
        'PSAvoidUsingWriteHost',
        # Invoke-Az / Test-Az pass az's own command line through
        'PSAvoidUsingPositionalParameters'
    )
}
