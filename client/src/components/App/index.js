import React, { Component, Fragment } from 'react';
import { HashRouter, Route } from 'react-router-dom';
import PropTypes from 'prop-types';
import { Trans, withNamespaces } from 'react-i18next';
import LoadingBar from 'react-redux-loading-bar';

import 'react-table/react-table.css';
import '../ui/Tabler.css';
import '../ui/ReactTable.css';
import './index.css';

import Header from '../../containers/Header';
import Dashboard from '../../containers/Dashboard';
import Settings from '../../containers/Settings';
import Filters from '../../containers/Filters';
import Logs from '../../containers/Logs';
import Footer from '../ui/Footer';
import Toasts from '../Toasts';
import Status from '../ui/Status';
import Topline from '../ui/Topline';
import i18n from '../../i18n';

class App extends Component {
    componentDidMount() {
        this.props.getDnsStatus();
        this.props.getVersion();
    }

    componentDidUpdate(prevProps) {
        if (this.props.dashboard.language !== prevProps.dashboard.language) {
            this.setLanguage();
        }
    }

    handleStatusChange = () => {
        this.props.enableDns();
    };

    setLanguage = () => {
        const { processing, language } = this.props.dashboard;

        if (!processing) {
            if (language) {
                i18n.changeLanguage(language);
            }
        }

        i18n.on('languageChanged', (lang) => {
            this.props.changeLanguage(lang);
        });
    }

    render() {
        const { dashboard } = this.props;
        const updateAvailable =
            !dashboard.processingVersions &&
            dashboard.isCoreRunning &&
            dashboard.isUpdateAvailable;
        const isExpiringCertificate = false;

        return (
            <HashRouter hashType='noslash'>
                <Fragment>
                    {updateAvailable &&
                        <Topline type="info">
                            {dashboard.announcement} <a href={dashboard.announcementUrl} target="_blank" rel="noopener noreferrer">Click here</a> for more info.
                        </Topline>
                    }
                    {isExpiringCertificate &&
                        <Topline type="warning">
                            <Trans components={[<a href="#settings" key="0">link</a>]}>
                                topline_expiring_certificate
                            </Trans>
                        </Topline>
                    }
                    <LoadingBar className="loading-bar" updateTime={1000} />
                    <Route component={Header} />
                    <div className="container container--wrap">
                        {!dashboard.processing && !dashboard.isCoreRunning &&
                            <div className="row row-cards">
                                <div className="col-lg-12">
                                    <Status handleStatusChange={this.handleStatusChange} />
                                </div>
                            </div>
                        }
                        {!dashboard.processing && dashboard.isCoreRunning &&
                            <Fragment>
                                <Route path="/" exact component={Dashboard} />
                                <Route path="/settings" component={Settings} />
                                <Route path="/filters" component={Filters} />
                                <Route path="/logs" component={Logs} />
                            </Fragment>
                        }
                    </div>
                    <Footer />
                    <Toasts />
                </Fragment>
            </HashRouter>
        );
    }
}

App.propTypes = {
    getDnsStatus: PropTypes.func,
    enableDns: PropTypes.func,
    dashboard: PropTypes.object,
    isCoreRunning: PropTypes.bool,
    error: PropTypes.string,
    getVersion: PropTypes.func,
    changeLanguage: PropTypes.func,
    encryption: PropTypes.object,
};

export default withNamespaces()(App);
