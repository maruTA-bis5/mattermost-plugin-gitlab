package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/plugin"

	"github.com/xanzy/go-gitlab"
	"github.com/mattermost/mattermost-server/model"
)

const COMMAND_HELP = `* |/gitlab connect| - Connect your Mattermost account to your GitLab account
* |/gitlab disconnect| - Disconnect your Mattermost account from your GitLab account
* |/gitlab todo| - Get a list of unread messages and pull requests awaiting your review
* |/gitlab subscribe owner/repo [features]| - Subscribe the current channel to receive notifications about opened pull requests and issues for a repository
  * |features| is a comma-delimited list of one or more the following:
    * issues - includes new issues
	* pulls - includes new pull requests
	* label:"<labelname>" - must include "pulls" or "issues" in feature list when using a label
  * Defaults to "pulls,issues"
* |/gitlab unsubscribe owner/repo| - Unsubscribe the current channel from a repository
* |/gitlab me| - Display the connected GitLab account
* |/gitlab settings [setting] [value]| - Update your user settings
  * |setting| can be "notifications" or "reminders"
  * |value| can be "on" or "off"`

func getCommand() *model.Command {
	return &model.Command{
		Trigger:          "gitlab",
		DisplayName:      "GitLab",
		Description:      "Integration with GitLab.",
		AutoComplete:     true,
		AutoCompleteDesc: "Available commands: connect, disconnect, todo, me, settings, subscribe, unsubscribe, help",
		AutoCompleteHint: "[command]",
	}
}

func getCommandResponse(responseType, text string) *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: responseType,
		Text:         text,
		Username:     GITLAB_USERNAME,
		IconURL:      GITLAB_ICON_URL,
		Type:         model.POST_DEFAULT,
	}
}

func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	split := strings.Fields(args.Command)
	command := split[0]
	parameters := []string{}
	action := ""
	if len(split) > 1 {
		action = split[1]
	}
	if len(split) > 2 {
		parameters = split[2:]
	}

	if command != "/gitlab" {
		return nil, nil
	}

	if action == "connect" {
		config := p.API.GetConfig()
		if config.ServiceSettings.SiteURL == nil {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Encountered an error connecting to GitLab."), nil
		}

		resp := getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, fmt.Sprintf("[Click here to link your GitLab account.](%s/plugins/gitlab/oauth/connect)", *config.ServiceSettings.SiteURL))
		return resp, nil
	}

	ctx := context.Background()
	var gitlabClient *gitlab.Client

	info, apiErr := p.getGitLabUserInfo(args.UserId)
	if apiErr != nil {
		text := "Unknown error."
		if apiErr.ID == API_ERROR_ID_NOT_CONNECTED {
			text = "You must connect your account to GitLab first. Either click on the GitLab logo in the bottom left of the screen or enter `/gitlab connect`."
		}
		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, text), nil
	}

	gitlabClient = p.gitlabConnect(*info.Token)

	switch action {
	case "subscribe":
		features := "pulls,issues"

		if len(parameters) == 0 {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Please specify a repository."), nil
		} else if len(parameters) > 1 {
			features = strings.Join(parameters[1:], " ")
		}

		repo := parameters[0]

		if err := p.Subscribe(context.Background(), gitlabClient, args.UserId, repo, args.ChannelId, features); err != nil {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, err.Error()), nil
		}

		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, fmt.Sprintf("Successfully subscribed to %s.", repo)), nil
	case "unsubscribe":
		if len(parameters) == 0 {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Please specify a repository."), nil
		}

		repo := parameters[0]

		if err := p.Unsubscribe(args.ChannelId, repo); err != nil {
			mlog.Error(err.Error())
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Encountered an error trying to unsubscribe. Please try again."), nil
		}

		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, fmt.Sprintf("Succesfully unsubscribed from %s.", repo)), nil
	case "disconnect":
		p.disconnectGitLabAccount(args.UserId)
		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Disconnected your GitHub account."), nil
	case "todo":
		text, err := p.GetToDo(ctx, info.GitLabUsername, gitlabClient)
		if err != nil {
			mlog.Error(err.Error())
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Encountered an error getting your to do items."), nil
		}
		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, text), nil
	case "me":
		gitUser, _, err := gitlabClient.Users.CurrentUser()
		if err != nil {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Encountered an error getting your GitLab profile."), nil
		}

		text := fmt.Sprintf("You are connected to GitLab as:\n# [![image](%s =40x40)](%s) [%s](%s)", gitUser.AvatarURL, p.BaseURL+"/"+gitUser.Username, gitUser.Username, p.BaseURL+"/"+gitUser.Username)
		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, text), nil
	case "help":
		text := "###### Mattermost GitLab Plugin - Slash Command Help\n" + strings.Replace(COMMAND_HELP, "|", "`", -1)

		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, text), nil
	case "settings":
		if len(parameters) < 2 {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Please specify both a setting and value. Use `/gitlab help` for more usage information."), nil
		}

		setting := parameters[0]
		if setting != SETTING_NOTIFICATIONS && setting != SETTING_REMINDERS {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Unknown setting."), nil
		}

		strValue := parameters[1]
		value := false
		if strValue == SETTING_ON {
			value = true
		} else if strValue != SETTING_OFF {
			return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Invalid value. Accepted values are: \"on\" or \"off\"."), nil
		}

		if setting == SETTING_NOTIFICATIONS {
			if value {
				p.storeGitLabToUserIDMapping(info.GitLabUsername, info.UserID)
			} else {
				p.API.KVDelete(info.GitLabUsername + GITLAB_USERNAME_KEY)
			}

			info.Settings.Notifications = value
		} else if setting == SETTING_REMINDERS {
			info.Settings.DailyReminder = value
		}

		p.storeGitLabUserInfo(info)

		return getCommandResponse(model.COMMAND_RESPONSE_TYPE_EPHEMERAL, "Settings updated."), nil
	}

	return nil, nil
}
